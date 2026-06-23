// Package engine — hls_copy_vod.go implements the COPY-VOD streaming model.
//
// The legacy VideoCopy path (buildHLSCopyArgs) runs ONE continuous ffmpeg
// `-c:v copy` writing an EVENT playlist that GROWS as ffmpeg outruns playback.
// Two problems for the viewer:
//   - the playlist's duration is unknown until ENDLIST → the seekbar total
//     keeps climbing ("1:23 / 5:10" then "1:48 / 20:55");
//   - you can't seek past the produced region → jumping to minute 50 means
//     waiting for the linear remux to reach it.
//
// COPY-VOD fixes both, Plex/Jellyfin-style, WITHOUT re-encoding video:
//   1. Index the source's keyframes once (ffprobe, key-frames only).
//   2. Group them into ~copyVODTargetSec segments, each STARTING on a keyframe
//      (copy can only cut at keyframes). Render a COMPLETE VOD playlist upfront
//      — every segment listed, real total duration, full seekbar from t=0.
//   3. Transcode each segment ON DEMAND when the player requests it
//      (`ffmpeg -copyts -ss start -i src -to end -c:v copy -f mpegts`),
//      keyframe-aligned. Seeking to minute 50 = generating one ~6 s segment
//      (~100 ms for copy), not waiting out a linear remux.
//
// Transport is MPEG-TS, NOT fMP4. fMP4 needs a single shared EXT-X-MAP init,
// but ffmpeg bakes the `-ss` start offset into each segment's init as an edit
// list (elst) — so a shared init mis-places every segment but the first (the
// player clamps them all to t=0; verified empirically). MPEG-TS segments are
// self-contained: each carries absolute PTS and no init, so independently-cut
// `-c copy` segments concatenate seamlessly across players (hls.js transmux +
// Safari native). The trade-off is codec reach: TS reliably carries H.264 +
// AAC/AC3 across browsers, but NOT HEVC (Apple HLS mandates fMP4 for HEVC) or
// AV1. So COPY-VOD is gated to H.264 sources; HEVC/AV1 copy (only chosen when
// the device declares native decode, i.e. Safari) stays on the legacy EVENT
// path — no regression, just no seek-anywhere there yet.
//
// Scope: H.264 copy sessions. LOCAL files get an exact keyframe index
// (frame-accurate seek). REMOTE sources (connector/IPTV/debrid) can't be
// keyframe-indexed without downloading the whole file, so they plan UNIFORM
// segments from the known duration instead — full duration + seek still work,
// but seek is GOP-rounded (the on-demand `-ss` input-seek lands on the nearest
// keyframe ≤ the boundary). A remote source without HTTP range support, or with
// no known duration, falls back to the legacy EVENT path (see StartHLSSession).
// Remote COPY-VOD also spawns a one-shot subtitle sidecar extractor, since its
// on-demand video segments never read the whole file the way the EVENT copy does.

package engine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// copyVODTargetSec is the nominal segment length for COPY-VOD. Larger than the
// encode mode's 2 s (segmentDurationFor) because every segment is a separate
// ffmpeg spawn — 6 s keeps the spawn count for a 2 h film near ~1200 instead of
// ~3600, while staying within Apple's recommended 6 s target. Actual durations
// vary: each segment runs from one source keyframe to the next group boundary.
const copyVODTargetSec = 6.0

// copyVODSegExt is the on-disk + playlist extension for COPY-VOD segments.
// MPEG-TS (.ts), not fMP4 (.m4s) — see the package comment.
const copyVODSegExt = ".ts"

// copyKeyframeIndexTimeout bounds the one-shot keyframe scan. A local 2 h film
// indexes in a few seconds; past this ceiling we give up and fall back to the
// legacy EVENT path rather than stranding the player on "Preparando sesión".
const copyKeyframeIndexTimeout = 45 * time.Second

// copyVODEligibleCodec reports whether a source video codec can ride COPY-VOD's
// MPEG-TS transport. H.264 only: TS carries it universally; HEVC needs fMP4
// (Apple HLS) and AV1 isn't a TS codec, so both fall back to legacy EVENT copy.
func copyVODEligibleCodec(videoCodec string) bool {
	switch strings.ToLower(videoCodec) {
	case "h264", "avc", "avc1":
		return true
	}
	return false
}

// indexKeyframes returns the sorted presentation timestamps (seconds) of every
// video keyframe in src.
//
// It reads PACKET headers (`-show_entries packet=pts_time,flags`) and keeps the
// ones flagged keyframe ("K") — a demux-only pass, NOT a decode. This is ~40×
// faster than `-skip_frame nokey` (which actually decodes each keyframe): a
// 24-min H.264 file indexes in ~0.3 s vs ~13 s, so even a 2-h film stays well
// under copyKeyframeIndexTimeout. Still a full demux of the container, hence
// local-file only. Returns an error if no usable keyframes are found.
func indexKeyframes(ctx context.Context, ffprobePath, src string) ([]float64, error) {
	ctx, cancel := context.WithTimeout(ctx, copyKeyframeIndexTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "packet=pts_time,flags",
		"-of", "csv=p=0",
		src,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("keyframe index: %w (%s)", err, strings.TrimSpace(errBuf.String()))
	}
	kfs := make([]float64, 0, 1024)
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// Each line is "pts_time,flags", e.g. "6.006000,K__". The flags field
		// carries "K" for a keyframe (RAP). Keep only those.
		line := strings.TrimSpace(sc.Text())
		comma := strings.IndexByte(line, ',')
		if comma < 0 {
			continue
		}
		ptsStr := strings.TrimSpace(line[:comma])
		flags := line[comma+1:]
		if !strings.Contains(flags, "K") {
			continue
		}
		if ptsStr == "" || ptsStr == "N/A" {
			continue
		}
		v, err := strconv.ParseFloat(ptsStr, 64)
		if err != nil || v < 0 {
			continue
		}
		kfs = append(kfs, v)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("keyframe index scan: %w", err)
	}
	if len(kfs) == 0 {
		return nil, errors.New("keyframe index: no keyframes found")
	}
	sort.Float64s(kfs)
	return kfs, nil
}

// planCopySegments turns a sorted keyframe list + total duration into the
// segment boundary table: starts[i]..starts[i+1] is segment i. starts[0] is
// always 0 and the final element is always duration, so len(starts)-1 ==
// segment count. Every interior boundary is a real keyframe, so an on-demand
// `-ss starts[i] -c copy` lands exactly (no mid-GOP cut).
//
// Greedy grouping: open a new segment at the first keyframe that is at least
// copyVODTargetSec past the current segment's start. A trailing sliver shorter
// than ~1 s is folded into the previous segment (a sub-1 s fragment is a
// needless extra spawn + a seekbar speck).
func planCopySegments(keyframes []float64, duration float64) []float64 {
	starts := []float64{0}
	last := 0.0
	for _, kf := range keyframes {
		// Skip keyframes at/below the current start (incl. the first ~0 one) and
		// anything at/after duration (a final-frame keyframe makes no segment).
		if kf <= last+0.001 || kf >= duration-0.001 {
			continue
		}
		if kf-last >= copyVODTargetSec {
			starts = append(starts, kf)
			last = kf
		}
	}
	// Close the table at the true duration. Fold a sub-1 s tail back into the
	// previous segment so we never list a near-empty final fragment.
	if duration-last < 1.0 && len(starts) > 1 {
		starts[len(starts)-1] = duration
	} else {
		starts = append(starts, duration)
	}
	return starts
}

// renderVideoPlaylistCopyVOD builds the complete VOD media playlist for a
// COPY-VOD session: every segment listed, exact per-segment EXTINF from the
// keyframe boundary table, ENDLIST present from the first fetch. The player
// learns the full timeline + total duration immediately and can seek anywhere.
// MPEG-TS segments → no EXT-X-MAP (no init), HLS version 3.
func renderVideoPlaylistCopyVOD(starts []float64) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	// TARGETDURATION must be >= the longest segment, rounded up. Segments are
	// keyframe-bounded so they can exceed the nominal target; compute the max.
	maxDur := 0.0
	for i := 0; i+1 < len(starts); i++ {
		if d := starts[i+1] - starts[i]; d > maxDur {
			maxDur = d
		}
	}
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int(maxDur)+1))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	for i := 0; i+1 < len(starts); i++ {
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", starts[i+1]-starts[i]))
		b.WriteString(fmt.Sprintf("seg-%d%s\n", i, copyVODSegExt))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// copyVODAudioArgs resolves the audio map + codec for a COPY-VOD segment,
// mirroring buildHLSCopyArgs exactly: copy AAC ≤2ch, re-encode everything else
// to AAC stereo 48k (so the TS always carries browser-safe AAC).
func copyVODAudioArgs(cfg HLSSessionConfig, probe *StreamProbe) []string {
	audioIdx := cfg.AudioIndex
	if audioIdx < 0 {
		audioIdx = 0
		for i, a := range probe.AudioTracks {
			if a.Default {
				audioIdx = i
				break
			}
		}
	}
	if n := len(probe.AudioTracks); n > 0 && audioIdx >= n {
		audioIdx = 0
	}
	args := []string{"-map", fmt.Sprintf("0:a:%d?", audioIdx)}
	audioCodec := probe.AudioCodec
	audioChannels := 0
	if audioIdx < len(probe.AudioTracks) {
		audioCodec = probe.AudioTracks[audioIdx].Codec
		audioChannels = probe.AudioTracks[audioIdx].Channels
	}
	if strings.EqualFold(audioCodec, "aac") && audioChannels > 0 && audioChannels <= 2 {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "192k", "-ar", "48000", "-ac", "2")
	}
	return args
}

// buildCopyVODSegmentArgs builds the ffmpeg invocation that produces ONE
// COPY-VOD MPEG-TS fragment for [start,end) into outPath. Correctness rests on:
//
//   - `-copyts`: keep the source's absolute timestamps so the segment's PTS
//     equals its real position. MPEG-TS carries them directly, so the player
//     places the segment correctly relative to its neighbours (no init needed).
//   - `-ss start` BEFORE `-i`: keyframe-accurate input seek. start is a real
//     keyframe time, so ffmpeg lands exactly on it (no preceding-GOP slop).
//   - `-to end`: an output limit against the copyts timestamps — stop at the
//     next boundary keyframe.
//
// Verified empirically (Wistoria S02E09, H.264+AAC): seg-N PTS = [start..end)
// with no gaps/overlap; hls.js shows the full duration + seeks anywhere.
func buildCopyVODSegmentArgs(cfg HLSSessionConfig, probe *StreamProbe, outPath string, start, end float64) []string {
	args := []string{
		"-y", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-copyts",
		"-ss", strconv.FormatFloat(start, 'f', 6, 64),
		"-i", cfg.sourceRef(),
		"-to", strconv.FormatFloat(end, 'f', 6, 64),
		"-map", "0:v:0",
	}
	args = append(args, copyVODAudioArgs(cfg, probe)...)
	args = append(args, "-c:v", "copy")
	args = append(args,
		"-muxdelay", "0", "-muxpreload", "0",
		"-f", "mpegts",
		outPath,
	)
	return args
}

// ensureCopySegment generates seg-idx.ts on demand if it is not already on
// disk, single-flighting concurrent requests for the SAME index so two player
// fetches (or a fetch racing a prewarm) never spawn duplicate ffmpegs.
func (s *HLSSession) ensureCopySegment(ctx context.Context, idx int) error {
	path := s.copySegPath(idx)
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
		return nil
	}
	gate := s.copyGenGate(idx)
	gate.Lock()
	defer gate.Unlock()
	// Re-check under the gate: a racer may have produced it while we waited.
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
		return nil
	}
	return s.generateCopySegment(ctx, idx)
}

// copySegPath is the on-disk path of COPY-VOD segment idx.
func (s *HLSSession) copySegPath(idx int) string {
	return filepath.Join(s.tmpDir, "video", fmt.Sprintf("seg-%d%s", idx, copyVODSegExt))
}

// copyGenGate returns the per-index mutex used to single-flight segment
// generation. A tiny map under copyGenMu — no external singleflight dep.
func (s *HLSSession) copyGenGate(idx int) *sync.Mutex {
	s.copyGenMu.Lock()
	defer s.copyGenMu.Unlock()
	if s.copyGen == nil {
		s.copyGen = make(map[int]*sync.Mutex)
	}
	g := s.copyGen[idx]
	if g == nil {
		g = &sync.Mutex{}
		s.copyGen[idx] = g
	}
	return g
}

// generateCopySegment runs ffmpeg to produce seg-idx.ts (written to a .tmp then
// atomically renamed, so a concurrent reader never sees a half-written file).
// Caller holds the per-index gate. Bounds: idx in [0, segmentCount).
func (s *HLSSession) generateCopySegment(ctx context.Context, idx int) error {
	if idx < 0 || idx >= s.segmentCount {
		return fmt.Errorf("hls: copy-vod segment %d out of range [0,%d)", idx, s.segmentCount)
	}
	start := s.copySegStarts[idx]
	end := s.copySegStarts[idx+1]
	final := s.copySegPath(idx)
	tmp := final + ".tmp"
	defer os.Remove(tmp) //nolint:errcheck — best-effort cleanup of a stale temp

	args := buildCopyVODSegmentArgs(s.cfg, s.probe, tmp, start, end)
	genCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(genCtx, s.cfg.Transcode.FFmpegPath, args...)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	startedAt := time.Now()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("hls: copy-vod seg-%d ffmpeg: %w (%s)", idx, err, strings.TrimSpace(errBuf.String()))
	}
	if fi, err := os.Stat(tmp); err != nil || fi.Size() == 0 {
		return fmt.Errorf("hls: copy-vod seg-%d not produced", idx)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("hls: copy-vod seg-%d publish: %w", idx, err)
	}
	log.Printf("[hls %s] copy-vod seg-%d ready (%.1f–%.1fs, %dms)",
		shortHLSID(s.cfg.SessionID), idx, start, end, time.Since(startedAt).Milliseconds())
	return nil
}

// startCopyVOD attempts to set up a COPY-VOD session: plan the segment table,
// render the complete VOD manifest (full duration + seek-anywhere), and — for a
// remote source — kick off a one-shot subtitle sidecar extractor. Returns false
// (no error) if the source can't be COPY-VOD'd (non-H.264 codec, no known
// duration, remote without HTTP range support, local keyframe-index failure) so
// the caller falls back to the legacy EVENT copy path. No video ffmpeg is
// spawned here — segments are produced lazily on first request.
func startCopyVOD(ctx context.Context, s *HLSSession) bool {
	// MPEG-TS transport carries H.264 universally but not HEVC/AV1 (see package
	// comment). Non-H.264 copy → legacy EVENT path (no regression).
	if !copyVODEligibleCodec(s.probe.VideoCodec) {
		log.Printf("[hls %s] copy-vod skipped: codec %q not TS-eligible — using EVENT copy",
			shortHLSID(s.cfg.SessionID), s.probe.VideoCodec)
		return false
	}

	var starts []float64
	switch {
	case s.cfg.SourceURL != "":
		// REMOTE (connector/IPTV/debrid): a keyframe index would download the
		// whole file, so plan UNIFORM segments from the known duration. The
		// on-demand `-ss` input-seek rounds DOWN to the nearest keyframe, so seek
		// is GOP-accurate (≤copyVODTargetSec off) while the full timeline shows
		// upfront. Needs a known duration + HTTP range support (else every
		// segment's -ss would re-read from byte 0); without either, EVENT copy.
		if s.durationSec <= 0 {
			log.Printf("[hls %s] copy-vod skipped: remote source has no known duration — using EVENT copy",
				shortHLSID(s.cfg.SessionID))
			return false
		}
		if !sourceSupportsRange(ctx, s.cfg.sourceRef()) {
			log.Printf("[hls %s] copy-vod skipped: remote source lacks HTTP range support — using EVENT copy",
				shortHLSID(s.cfg.SessionID))
			return false
		}
		starts = planUniformSegments(s.durationSec)
		// The on-demand video segments never read the whole file, so subtitles
		// ride a separate one-shot pass that fills subs/ progressively.
		startCopyVODSubtitles(s)
	case s.cfg.SourcePath != "":
		// LOCAL: a fast demux-only keyframe scan gives exact keyframe boundaries
		// (frame-accurate seek, no GOP rounding).
		kfs, err := indexKeyframes(ctx, s.cfg.Transcode.FFprobePath, s.cfg.sourceRef())
		if err != nil {
			log.Printf("[hls %s] copy-vod keyframe index failed (%v) — using EVENT copy",
				shortHLSID(s.cfg.SessionID), err)
			return false
		}
		starts = planCopySegments(kfs, s.durationSec)
	default:
		return false
	}

	if len(starts) < 2 {
		log.Printf("[hls %s] copy-vod planning yielded no segments — using EVENT copy",
			shortHLSID(s.cfg.SessionID))
		return false
	}
	s.copyVOD = true
	s.copySegStarts = starts
	s.segmentCount = len(starts) - 1
	s.manifestVideo = renderVideoPlaylistCopyVOD(starts)
	s.manifestRoot = renderMasterPlaylistCopy(s.probe)
	// No live encoder: every segment is generated on demand. Mark the session
	// "ready" (readyMax past the writer start) so watchSessionReady flips the
	// "Preparando…" overlay immediately and the player starts fetching.
	s.readyMu.Lock()
	s.readyMax = s.segmentCount
	s.exited = true
	s.readyMu.Unlock()
	mode := "local/keyframe"
	if s.cfg.SourceURL != "" {
		mode = "remote/uniform"
	}
	log.Printf("[hls %s] copy-vod: %d segments, %.1fs (on-demand -c:v copy, mpegts, %s)",
		shortHLSID(s.cfg.SessionID), s.segmentCount, s.durationSec, mode)
	return true
}

// planUniformSegments plans a COPY-VOD segment table at fixed copyVODTargetSec
// boundaries across 0..duration — for REMOTE sources where a real keyframe index
// isn't affordable. Same shape as planCopySegments (starts[0]==0, final element
// ==duration, len-1 == segment count); the difference is the interior boundaries
// are wall-clock multiples, not keyframes, so an on-demand `-ss` input-seek
// rounds down to the nearest preceding keyframe. A sub-1 s trailing sliver is
// folded into the last segment (no near-empty final fragment).
func planUniformSegments(duration float64) []float64 {
	if duration <= 0 {
		return nil
	}
	starts := []float64{0}
	for t := copyVODTargetSec; t < duration-1.0; t += copyVODTargetSec {
		starts = append(starts, t)
	}
	starts = append(starts, duration)
	return starts
}

// sourceSupportsRange reports whether url answers an HTTP byte-range request
// (status 206). COPY-VOD on a remote source seeks every segment with `-ss`,
// which only stays cheap when the server honours Range — otherwise each segment
// re-reads from byte 0. A tight timeout keeps a dead/slow panel from stalling
// session start; any error reports false (→ caller uses the EVENT copy path).
func sourceSupportsRange(ctx context.Context, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Range", "bytes=0-1")
	// IPTV panels commonly gate on a player UA; match a VLC-class client so the
	// probe reflects what the segment ffmpeg can actually pull, not a Go default.
	req.Header.Set("User-Agent", "VLC/3.0.20 LibVLC/3.0.20")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusPartialContent
}

// startCopyVODSubtitles spawns a background ffmpeg that reads the remote source
// ONCE and writes a WebVTT sidecar per TEXT subtitle track (subs/s<idx>.vtt),
// mirroring the EVENT copy path's in-pass sidecars — needed because COPY-VOD's
// on-demand segments never read the whole file. `-flush_packets 1` streams each
// cue to disk so the sidecar fills progressively (ServeSubtitleVTT serves what's
// read so far). The extractor starts a few seconds late so the first video
// segment isn't contended on a single-line panel, and its cancel is stored on
// s.cancel so Close() kills it. No-op when the source has no text subtitles.
func startCopyVODSubtitles(s *HLSSession) {
	var outs []string
	for _, sb := range s.probe.SubtitleTracks {
		if !sb.IsTextSubtitle() {
			continue
		}
		outs = append(outs,
			"-map", fmt.Sprintf("0:s:%d?", sb.Index),
			"-c:s", "webvtt",
			"-flush_packets", "1",
			"-f", "webvtt",
			filepath.Join(s.tmpDir, "subs", fmt.Sprintf("s%d.vtt", sb.Index)),
		)
	}
	if len(outs) == 0 {
		return
	}
	args := []string{
		"-y", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "5",
		"-rw_timeout", "30000000",
		"-i", s.cfg.sourceRef(),
	}
	args = append(args, outs...)

	ffCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	go func() {
		// Yield the panel to the first video segment before opening a second read.
		select {
		case <-ffCtx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		cmd := exec.CommandContext(ffCtx, s.cfg.Transcode.FFmpegPath, args...)
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil && ffCtx.Err() == nil {
			log.Printf("[hls %s] copy-vod subtitle extractor: %v (%s)",
				shortHLSID(s.cfg.SessionID), err, strings.TrimSpace(errBuf.String()))
			return
		}
		if ffCtx.Err() == nil {
			log.Printf("[hls %s] copy-vod subtitle sidecars complete", shortHLSID(s.cfg.SessionID))
		}
	}()
}
