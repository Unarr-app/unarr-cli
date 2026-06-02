package mediainfo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Sidecar cache: unarr stores extracted artifacts (WebVTT subtitles, thumbnail
// frames) in a hidden ".unarr" directory NEXT TO the media file, not in the XDG
// cache. Keeping them beside the content means they travel with the file and
// survive a cache-dir wipe, and the scan-time prewarm and the on-demand stream
// handlers share the exact same path scheme — so a subtitle/thumbnail extracted
// during a library scan is reused verbatim at play time (no re-extraction, no
// 60s-HTTP-timeout failures on huge remuxes).
//
// Everything here is best-effort: a read-only media mount just means no cache
// (the on-demand path still works), and a stale cache (media replaced) is
// detected by mtime and ignored.

const sidecarDirName = ".unarr"

// IsTextSubtitleCodec reports whether a subtitle codec can be extracted to
// WebVTT (text-based). Mirrors engine.ProbeSubtitleTrack.IsTextSubtitle and the
// web's isTextSubtitleCodec whitelist — bitmap subs (PGS/DVB/VOBSUB) are burned
// in, not extracted. Defined here (the leaf media package) so both the stream
// handlers and the scan-time prewarm classify codecs identically.
func IsTextSubtitleCodec(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "subrip", "srt", "ass", "ssa", "webvtt", "mov_text", "text":
		return true
	default:
		return false
	}
}

// sidecarDir returns the hidden per-folder cache directory for a media file.
func sidecarDir(mediaPath string) string {
	return filepath.Join(filepath.Dir(mediaPath), sidecarDirName)
}

// subtitleCachePath is the cached WebVTT path for subtitle stream `index`
// (0-based, matching ffmpeg's 0:s:N ordering) of mediaPath.
func subtitleCachePath(mediaPath string, index int) string {
	return filepath.Join(sidecarDir(mediaPath), fmt.Sprintf("%s.s%d.vtt", filepath.Base(mediaPath), index))
}

// thumbnailCachePath is the cached JPEG path for a single frame at posSec
// (rounded to whole seconds) and the given width. The handler and the scan
// prewarm round identically so the same logical frame maps to one cache file.
func thumbnailCachePath(mediaPath string, posSec float64, width int) string {
	sec := int(math.Round(posSec))
	if sec < 0 {
		sec = 0
	}
	return filepath.Join(sidecarDir(mediaPath), fmt.Sprintf("%s.t%dw%d.jpg", filepath.Base(mediaPath), sec, width))
}

// sidecarFresh reports whether a cache file exists and is at least as new as the
// media file. A re-download/replace bumps the media mtime and invalidates the
// stale sidecar so we re-extract.
func sidecarFresh(cachePath, mediaPath string) bool {
	cfi, err := os.Stat(cachePath)
	if err != nil {
		return false
	}
	mfi, err := os.Stat(mediaPath)
	if err != nil {
		return false
	}
	return !cfi.ModTime().Before(mfi.ModTime())
}

// writeSidecar atomically writes data to a sidecar path (temp + rename), creating
// the hidden dir if needed. Returns an error the caller logs and continues on
// (e.g. a read-only mount) — caching is never required for correctness.
func writeSidecar(path string, data []byte) error {
	if len(data) == 0 {
		return errors.New("refusing to cache empty artifact")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ReadCachedSubtitle returns the cached WebVTT for (mediaPath, index) when a
// fresh sidecar exists. ok=false means the caller should extract on demand.
func ReadCachedSubtitle(mediaPath string, index int) ([]byte, bool) {
	p := subtitleCachePath(mediaPath, index)
	if !sidecarFresh(p, mediaPath) {
		return nil, false
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return b, true
}

// WriteCachedSubtitle stores extracted WebVTT next to the media. Best-effort.
func WriteCachedSubtitle(mediaPath string, index int, vtt []byte) error {
	return writeSidecar(subtitleCachePath(mediaPath, index), vtt)
}

// ExtractSubtitleVTT runs ffmpeg to convert subtitle stream `index` of mediaPath
// to WebVTT bytes. Shared by the on-demand /sub handler and the scan-time prewarm
// so both produce identical output. The caller owns the ctx deadline: the handler
// uses a short HTTP-bound timeout; the prewarm uses a generous one (a full text
// track on a multi-GB remux can take minutes to demux).
func ExtractSubtitleVTT(ctx context.Context, ffmpegPath, mediaPath string, index int) ([]byte, error) {
	// -map 0:s:<index>? selects the Nth subtitle stream (non-fatal if absent);
	// -c:s webvtt converts srt/ass/mov_text/etc. to WebVTT on stdout.
	args := []string{
		"-nostdin",
		"-loglevel", "error",
		"-i", mediaPath,
		"-map", fmt.Sprintf("0:s:%d?", index),
		"-c:s", "webvtt",
		"-f", "webvtt",
		"-",
	}
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg subtitle extract: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if len(out) == 0 {
		return nil, errors.New("ffmpeg produced no subtitle output")
	}
	return out, nil
}

// ExtractSubtitlesVTTMulti extracts several text subtitle streams in a SINGLE
// ffmpeg pass. The expensive part of subtitle extraction is demuxing the whole
// container (subtitle packets are interleaved across the runtime), so a 60GB
// remux with N text tracks costs N full reads when done one index at a time —
// here it's one read for all of them. Returns index→WebVTT for the streams that
// produced output (an empty stream is simply absent, not an error). ffmpeg can't
// multiplex several outputs onto stdout, so it writes per-track temp files which
// are read back; callers cache them via WriteCachedSubtitle.
func ExtractSubtitlesVTTMulti(ctx context.Context, ffmpegPath, mediaPath string, indices []int) (map[int][]byte, error) {
	if len(indices) == 0 {
		return nil, nil
	}
	tmpDir, err := os.MkdirTemp("", "unarr-subs-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	args := []string{"-nostdin", "-loglevel", "error", "-i", mediaPath}
	tmp := make(map[int]string, len(indices))
	for _, idx := range indices {
		f := filepath.Join(tmpDir, fmt.Sprintf("s%d.vtt", idx))
		tmp[idx] = f
		// One output file per stream; output options precede each output path.
		args = append(args, "-map", fmt.Sprintf("0:s:%d?", idx), "-c:s", "webvtt", "-f", "webvtt", "-y", f)
	}

	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	// Run it at IDLE I/O priority: this single ~14 min sequential read of a huge
	// remux must not starve live streaming off the same disk/NFS.
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg multi-subtitle start: %w", err)
	}
	setIdleIOPriority(cmd.Process.Pid)
	runErr := cmd.Wait()

	// If ffmpeg was KILLED (ctx deadline/cancel on a file too big to finish in
	// time), any temp file it left is a truncated WebVTT — a valid header plus
	// partial cues, so it passes the len>0 check and would be cached as a
	// silently-incomplete track until the media's mtime changes. Distrust all
	// output in that case. A clean non-zero exit (e.g. one empty/corrupt stream)
	// still leaves good complete files for the other tracks, so we keep those.
	var exitErr *exec.ExitError
	killed := runErr != nil && errors.As(runErr, &exitErr) && !exitErr.ProcessState.Exited()

	out := make(map[int][]byte, len(indices))
	if !killed {
		for idx, f := range tmp {
			if b, rerr := os.ReadFile(f); rerr == nil && len(b) > 0 {
				out[idx] = b
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("ffmpeg multi-subtitle extract: no usable output (err=%v): %s", runErr, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ReadCachedThumbnail returns the cached JPEG for (mediaPath, posSec, width) when
// a fresh sidecar exists. ok=false means extract on demand.
func ReadCachedThumbnail(mediaPath string, posSec float64, width int) ([]byte, bool) {
	p := thumbnailCachePath(mediaPath, posSec, width)
	if !sidecarFresh(p, mediaPath) {
		return nil, false
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return b, true
}

// WriteCachedThumbnail stores an extracted JPEG frame next to the media. Best-effort.
func WriteCachedThumbnail(mediaPath string, posSec float64, width int, jpeg []byte) error {
	return writeSidecar(thumbnailCachePath(mediaPath, posSec, width), jpeg)
}

// ExtractThumbnailJPEG decodes ONE frame at posSec, scaled to `width`, as JPEG
// bytes. Mirrors engine.buildThumbnailArgs so the scan-time prewarm produces
// frames byte-identical to the on-demand handler (`-ss` before `-i` = fast
// input/keyframe seek). Shared by the prewarm; the handler keeps its own inline
// extraction (covered by thumbnail_test.go) and only reuses the cache helpers.
func ExtractThumbnailJPEG(ctx context.Context, ffmpegPath, mediaPath string, posSec float64, width int) ([]byte, error) {
	args := []string{
		"-nostdin",
		"-loglevel", "error",
		"-ss", strconv.FormatFloat(posSec, 'f', 3, 64),
		"-i", mediaPath,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", width),
		"-an", "-sn",
		"-f", "mjpeg",
		"pipe:1",
	}
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg thumbnail start: %w", err)
	}
	setIdleIOPriority(cmd.Process.Pid) // background prewarm yields I/O to live playback
	err := cmd.Wait()
	out := stdout.Bytes()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg thumbnail extract: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if len(out) == 0 {
		return nil, errors.New("ffmpeg produced no thumbnail (seek past EOF?)")
	}
	return out, nil
}
