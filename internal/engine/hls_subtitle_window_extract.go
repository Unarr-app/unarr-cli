package engine

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// subtitleClockSlackSec is how far past a window's end the video clock runs
// before the read is cut: cues are muxed near, not exactly at, the video of the
// same timestamp.
const subtitleClockSlackSec = 2.0

// subtitleFirstWindowWait caps how long a sidecar request waits for the first
// window. A loading <track> holds the media element below HAVE_FUTURE_DATA, so
// this is also a cap on delayed playback start; past it the track is served
// with whatever exists and the player tops it up (subs/status.json).
const subtitleFirstWindowWait = 6 * time.Second

// subtitleWindowArgs builds the ffmpeg invocation reading [start, end).
//
// Output `-to` alone does not bound the READ: a sparse track (signs & songs)
// only finishes when a cue past the window shows up, which may be minutes of
// file away — or never. So the video is stream-copied into a framecrc output on
// stdout: one line per packet carrying its timestamp, i.e. a clock telling
// runFFmpegUntil how far the read has got. (`-progress` is no substitute: under
// -copyts its out_time is rebased differently from one ffmpeg release to the
// next, and was seen ending on a negative value.)
//
// -copyts -start_at_zero keeps cue times on the zero-based source timeline, the
// one the former whole-file pass produced. Without -copyts the WebVTT muxer
// shifts the whole window by however much its first cue precedes the seek point
// (measured: +0.53 s on one window of the field sample, exact on the others).
func subtitleWindowArgs(src, dir string, tracks []int, start, end float64) []string {
	stamp := func(sec float64) string { return strconv.FormatFloat(sec, 'f', 3, 64) }
	args := []string{"-y", "-nostdin", "-hide_banner", "-loglevel", "error"}
	if strings.HasPrefix(src, "http") { // protocol options a file input rejects
		args = append(args,
			"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "5",
			"-rw_timeout", "30000000")
	}
	args = append(args, "-copyts", "-start_at_zero")
	if start > 0 {
		args = append(args, "-ss", stamp(start))
	}
	args = append(args, "-i", src,
		"-map", "0:v:0", "-c", "copy",
		"-to", stamp(end+subtitleClockSlackSec+1),
		"-f", "framecrc", "pipe:1")
	for _, idx := range tracks {
		args = append(args,
			"-map", fmt.Sprintf("0:s:%d?", idx),
			"-c:s", "webvtt",
			"-flush_packets", "1",
			"-to", stamp(end),
			"-f", "webvtt",
			filepath.Join(dir, fmt.Sprintf("s%d.vtt", idx)),
		)
	}
	return args
}

// frameClock turns framecrc output into a position. Lines look like
//
//	#tb 0: 1/1000
//	0,     238238,     238238,       41,   518064, 0x4e18f41b
//
// (stream, dts, pts, duration, size, crc; timestamps in #tb units).
type frameClock struct {
	num, den int64
}

// position is the timestamp carried by one line; ok=false for anything else.
func (c *frameClock) position(line string) (time.Duration, bool) {
	if tb, ok := strings.CutPrefix(line, "#tb 0:"); ok {
		num, den, _ := strings.Cut(strings.TrimSpace(tb), "/")
		c.num, _ = strconv.ParseInt(num, 10, 64)
		c.den, _ = strconv.ParseInt(den, 10, 64)
		return 0, false
	}
	fields := strings.Split(line, ",")
	if c.den <= 0 || len(fields) < 3 || strings.HasPrefix(line, "#") {
		return 0, false
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64) // dts: monotonic
	if err != nil {
		return 0, false
	}
	return time.Duration(float64(ts) * float64(c.num) / float64(c.den) * float64(time.Second)), true
}

// runFFmpegUntil runs ffmpeg and ends it once the frame clock on its stdout
// reaches limit (a success), or lets it finish by itself if it never does.
func runFFmpegUntil(ctx context.Context, ffmpeg string, args []string, limit time.Duration) error {
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(runCtx, ffmpeg, args...)
	winproc.HideWindow(cmd)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	reached := false
	var clock frameClock
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if pos, ok := clock.position(sc.Text()); ok && pos >= limit {
			reached = true
			stop()
			break
		}
	}
	err = cmd.Wait()
	switch {
	case reached:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		return fmt.Errorf("%w (%s)", err, strings.TrimSpace(errBuf.String()))
	}
	return nil
}

// extractSubtitleWindow is the session's extractWindowFunc.
func (s *HLSSession) extractSubtitleWindow(ctx context.Context, start, dur float64) (map[int][]vttCue, error) {
	dir := filepath.Join(s.tmpDir, "subs", fmt.Sprintf("w%d", int(start)))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	tracks := s.textSubtitleTracks()
	end := start + dur
	// Through the proxy's background lane: it only gets bytes while no video
	// segment is being fetched, so extraction never costs the viewer a stall.
	args := subtitleWindowArgs(s.copyBulkSource(), dir, tracks, start, end)
	limit := time.Duration((end + subtitleClockSlackSec) * float64(time.Second))
	if err := runFFmpegUntil(ctx, s.cfg.Transcode.FFmpegPath, args, limit); err != nil {
		return nil, err
	}
	last := end >= s.durationSec
	out := make(map[int][]vttCue, len(tracks))
	for _, idx := range tracks {
		vtt, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("s%d.vtt", idx))) //nolint:gosec // G304: session tmpDir + probe-derived index.
		if err != nil {
			continue // `-map 0:s:N?` — a track ffmpeg could not map has no file
		}
		// ffmpeg's ass→webvtt leaks vector-drawing paths as cue text.
		cues := parseVTTCues(mediainfo.FilterVTTDrawingCues(vtt))
		out[idx] = windowCues(cues, start, end, last)
	}
	return out, nil
}

// windowCues keeps the cues that START inside [from, to). The read covers more
// than the window on both sides (the seek lands on an earlier keyframe, the
// clock overshoots the end) and a cue must belong to exactly one window. The
// last window has no successor, so it keeps everything after from.
func windowCues(cues []vttCue, from, to float64, last bool) []vttCue {
	sec := func(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
	kept := cues[:0]
	for _, c := range cues {
		if c.start < sec(from) || (c.start >= sec(to) && !last) {
			continue
		}
		kept = append(kept, c)
	}
	return kept
}

func (s *HLSSession) textSubtitleTracks() []int {
	var tracks []int
	for _, sb := range s.probe.SubtitleTracks {
		if sb.IsTextSubtitle() && !sb.External { // externals have no 0:s:N
			tracks = append(tracks, sb.Index)
		}
	}
	return tracks
}

// serveWindowedSubtitleVTT serves track idx from the windows extracted so far.
// One-shot, never held open — see ServeSubtitleVTT.
func (s *HLSSession) serveWindowedSubtitleVTT(w http.ResponseWriter, r *http.Request, idx int) {
	wait := time.NewTimer(subtitleFirstWindowWait)
	defer wait.Stop()
	select {
	case <-s.subWin.first:
	case <-wait.C:
	case <-s.copyCtx.Done():
		http.Error(w, "subtitle track unavailable", http.StatusServiceUnavailable)
		return
	case <-r.Context().Done():
		return
	}
	vtt := renderVTT(s.subWin.snapshot(idx))
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(vtt)))
	_, _ = w.Write(vtt)
}
