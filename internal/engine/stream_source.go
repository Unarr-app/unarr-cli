package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// streamSource abstracts the byte source consumed by the HLS transcoder.
// Two implementations:
//   - diskFileSource — direct passthrough of the on-disk file.
//   - transcodeSource — ffmpeg writes a fragmented MP4 to a temp file in
//     real time; reads block briefly when callers ask for bytes ahead of
//     the writer.
type streamSource interface {
	ReadAt(p []byte, off int64) (int, error)
	// Size returns the currently known size. For transcoded sources this
	// grows as ffmpeg produces output; on Final() it's the final size.
	Size() int64
	// Final reports whether the source size is now stable (passthrough is
	// always final, transcoder becomes final when ffmpeg exits).
	Final() bool
	// EstimatedSize returns the final size we expect to converge on. For
	// passthrough it's the same as Size(). For transcoder it's a bitrate
	// × duration estimate so the browser scrubber has something to anchor
	// on; the real size will differ ±20%.
	EstimatedSize() int64
	FileName() string
	Transcoded() bool
	Close() error
}

// ─────────────────────────────────────────────────────────────────────────────
// disk passthrough
// ─────────────────────────────────────────────────────────────────────────────

type diskFileSource struct {
	f    *os.File
	size int64
	name string
}

func newDiskFileSource(path string) (*diskFileSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("stream source: open %s: %w", path, err)
	}
	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stream source: stat %s: %w", path, err)
	}
	return &diskFileSource{f: f, size: stat.Size(), name: filepath.Base(path)}, nil
}

func (d *diskFileSource) ReadAt(p []byte, off int64) (int, error) {
	return d.f.ReadAt(p, off)
}
func (d *diskFileSource) Size() int64          { return d.size }
func (d *diskFileSource) Final() bool          { return true }
func (d *diskFileSource) EstimatedSize() int64 { return d.size }
func (d *diskFileSource) FileName() string     { return d.name }
func (d *diskFileSource) Transcoded() bool     { return false }
func (d *diskFileSource) Close() error         { return d.f.Close() }

// ─────────────────────────────────────────────────────────────────────────────
// transcode source — ffmpeg → tmp file
// ─────────────────────────────────────────────────────────────────────────────

type transcodeSource struct {
	tmpPath  string
	tmpFile  *os.File
	cmd      *Transcoder
	name     string
	estimate int64

	ctx       context.Context
	notify    chan struct{} // size grew or final flipped; cap=1, non-blocking send
	size      atomic.Int64
	final     atomic.Bool
	failure   atomic.Pointer[error]
	startedAt time.Time
}

const (
	// readBlockTimeout caps how long ReadAt waits for bytes that haven't
	// been transcoded yet before returning EOF/io.ErrUnexpectedEOF. The
	// pump treats EOF as "respond with whatever we have so far + RangeEnd"
	// so the browser can re-request once more bytes appear.
	readBlockTimeout = 30 * time.Second
)

func newTranscodeSource(
	ctx context.Context,
	srcPath string,
	probe *StreamProbe,
	action TranscodeAction,
	opts TranscodeOpts,
	displayName string,
) (*transcodeSource, error) {
	tmpFile, err := os.CreateTemp("", "tc-stream-*.mp4")
	if err != nil {
		return nil, fmt.Errorf("transcode source: tmp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()

	args := buildFFmpegArgs(srcPath, opts)
	// Override -f mp4 pipe:1 with output to our tmp file path (last 3 args).
	if len(args) >= 3 && args[len(args)-1] == "pipe:1" {
		args[len(args)-1] = tmpPath
	}

	// Spawn ffmpeg directly (not via NewTranscoder pipe) so it writes to
	// disk in real time. We re-use the rest of TranscodeOpts wiring.
	cmd, err := startTranscoderToFile(ctx, opts.FFmpegPath, args, nil)
	if err != nil {
		os.Remove(tmpPath)
		return nil, err
	}

	// Size estimate for the scrubber timeline. A copy remux (video not
	// re-encoded) lands within container overhead of the source file, so the
	// source size is a far better estimate than bitrate×duration — use it.
	// A real transcode re-encodes, so fall back to the bitrate×duration model.
	var estimate int64
	switch action {
	case ActionPassthrough, ActionRemux, ActionRemuxAudio:
		if fi, statErr := os.Stat(srcPath); statErr == nil {
			estimate = fi.Size()
		}
	}
	if estimate <= 0 {
		estimate = estimateOutputSize(probe, opts)
	}

	t := &transcodeSource{
		tmpPath:   tmpPath,
		cmd:       cmd,
		name:      displayName,
		estimate:  estimate,
		ctx:       ctx,
		notify:    make(chan struct{}, 1),
		startedAt: time.Now(),
	}

	// Re-open the tmp file for reading; ffmpeg keeps writing to it.
	rf, err := os.Open(tmpPath)
	if err != nil {
		_ = cmd.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("transcode source: reopen tmp: %w", err)
	}
	t.tmpFile = rf

	go t.watchSize(ctx)
	go t.watchExit()
	return t, nil
}

// NewRemuxSource starts an ffmpeg `-c copy` remux of srcPath into a growing
// fragmented-MP4 temp file and returns it as a GrowingSource for /stream
// (hueco #3 / 3b). The video + audio are copied (never re-encoded), so this is
// only valid when the codecs are already browser-native (h264 + aac) and only
// the container needs changing — the web's decidePlayMethod enforces that
// before sending PlayMethod="remux". The browser plays the result progressively
// via byte-range. Caller MUST Close() it (kills ffmpeg + removes the temp file).
func NewRemuxSource(ctx context.Context, srcPath string, probe *StreamProbe, ffmpegPath, displayName string) (GrowingSource, error) {
	// Audio: copy when already AAC; otherwise transcode to AAC (ActionRemuxAudio).
	// Either way the VIDEO is copied — the expensive part is never re-encoded.
	// This lets remux cover the very common h264+AC3/DTS mkv case (hueco #3 / 3c),
	// not just h264+AAC.
	action := ActionRemux
	if probe != nil && probe.AudioCodec != "" && probe.AudioCodec != "aac" {
		action = ActionRemuxAudio
	}
	opts := TranscodeOpts{Action: action, FFmpegPath: ffmpegPath}
	// HEVC muxed into MP4 must carry the hvc1 tag or Apple/Safari won't decode
	// it (hueco #3 / 3c). h264 (avc1) needs no override.
	if probe != nil && probe.VideoCodec == "hevc" {
		opts.VideoTag = "hvc1"
	}
	return newTranscodeSource(ctx, srcPath, probe, action, opts, displayName)
}

// signalNotify wakes any goroutine blocked in ReadAt. Non-blocking: if a
// notification is already pending the new event is folded into it (callers
// always re-check size + final after waking, so a coalesced signal still
// produces correct behaviour).
func (t *transcodeSource) signalNotify() {
	select {
	case t.notify <- struct{}{}:
	default:
	}
}

// watchSize polls the temp file size every 200 ms and wakes any blocked
// ReadAt callers once new bytes arrive.
func (t *transcodeSource) watchSize(ctx context.Context) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.signalNotify()
			return
		case <-ticker.C:
		}
		if t.final.Load() {
			t.signalNotify()
			return
		}
		stat, err := os.Stat(t.tmpPath)
		if err != nil {
			continue
		}
		current := stat.Size()
		if current > t.size.Load() {
			if t.size.Load() == 0 && current > 0 {
				// TTFF diagnosis: how long from ffmpeg spawn to the first
				// fMP4 bytes (init + first fragment) landing — the floor on
				// when /stream can serve anything playable.
				log.Printf("[stream] %s first fMP4 bytes after %v (%d KB)",
					t.name, time.Since(t.startedAt).Round(time.Millisecond), current/1024)
			}
			t.size.Store(current)
			t.signalNotify()
		}
	}
}

// watchExit waits for ffmpeg to exit (via Transcoder's single-Wait goroutine)
// and locks in the final size. A kill triggered by Close() is NOT a failure.
func (t *transcodeSource) watchExit() {
	<-t.cmd.Done()
	err := t.cmd.WaitErr()
	if err != nil && !t.cmd.IsClosing() {
		failure := fmt.Errorf("ffmpeg exited: %w (%s)", err, t.cmd.Stderr())
		t.failure.Store(&failure)
	}
	if stat, err := os.Stat(t.tmpPath); err == nil {
		t.size.Store(stat.Size())
	}
	t.final.Store(true)
	t.signalNotify()
}

// loadFailure returns the current failure (or nil) without taking a lock.
func (t *transcodeSource) loadFailure() error {
	if p := t.failure.Load(); p != nil {
		return *p
	}
	return nil
}

func (t *transcodeSource) ReadAt(p []byte, off int64) (int, error) {
	if err := t.loadFailure(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, fmt.Errorf("transcode source: negative offset %d", off)
	}
	want := int64(len(p))

	deadline := time.Now().Add(readBlockTimeout)
	for {
		if t.final.Load() {
			break
		}
		size := t.size.Load()
		// Overflow-safe form of "off + want <= size":
		if size >= off && size-off >= want {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		wait := 500 * time.Millisecond
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-t.ctx.Done():
			return 0, t.ctx.Err()
		case <-t.notify:
		case <-time.After(wait):
		}
	}

	if err := t.loadFailure(); err != nil {
		return 0, err
	}

	n, err := t.tmpFile.ReadAt(p, off)
	// On a growing file ReadAt returns io.EOF when reading past current size.
	// Translate that into "send what we have, RangeEnd will follow" by
	// returning (n, nil) so the pump treats the data as a partial chunk and
	// caller re-requests once more bytes appear. Only true EOF (final=true)
	// propagates as io.EOF.
	if err == io.EOF && !t.final.Load() {
		if n > 0 {
			return n, nil
		}
		return 0, errors.New("transcode source: read timed out waiting for ffmpeg output")
	}
	return n, err
}

func (t *transcodeSource) Size() int64 { return t.size.Load() }
func (t *transcodeSource) Final() bool { return t.final.Load() }
func (t *transcodeSource) EstimatedSize() int64 {
	if t.final.Load() {
		return t.size.Load()
	}
	return t.estimate
}
func (t *transcodeSource) FileName() string {
	// Output is always fragmented MP4 regardless of source extension.
	return strings.TrimSuffix(t.name, filepath.Ext(t.name)) + ".mp4"
}
func (t *transcodeSource) Transcoded() bool { return true }
func (t *transcodeSource) Close() error {
	var errs []error
	if err := t.cmd.Close(); err != nil {
		errs = append(errs, err)
	}
	if t.tmpFile != nil {
		if err := t.tmpFile.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if t.tmpPath != "" {
		if err := os.Remove(t.tmpPath); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// estimateOutputSize converts probed bitrate × duration into a byte estimate
// so the browser scrubber has something to anchor on while transcoding.
func estimateOutputSize(probe *StreamProbe, opts TranscodeOpts) int64 {
	if probe == nil || probe.DurationSec <= 0 {
		return 0
	}
	videoKbps := parseBitrateKbps(opts.VideoBitrate, 5000)
	audioKbps := parseBitrateKbps(opts.AudioBitrate, 192)
	totalKbps := videoKbps + audioKbps
	bytesPerSec := int64(totalKbps) * 1000 / 8
	return int64(probe.DurationSec) * bytesPerSec
}

// parseBitrateKbps converts ffmpeg-style bitrate strings ("5M", "192k") to
// kilobits per second. Unknown formats fall back to fallback.
func parseBitrateKbps(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	last := s[len(s)-1]
	num := s
	mult := 1
	switch last {
	case 'k', 'K':
		num = s[:len(s)-1]
	case 'M', 'm':
		num = s[:len(s)-1]
		mult = 1000
	default:
		// already in bps? treat as kbps
	}
	v := 0
	for _, c := range num {
		if c < '0' || c > '9' {
			return fallback
		}
		v = v*10 + int(c-'0')
	}
	if v == 0 {
		return fallback
	}
	return v * mult
}
