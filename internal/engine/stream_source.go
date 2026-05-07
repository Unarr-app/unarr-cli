package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// streamSource abstracts the byte source served over the WebRTC DataChannel.
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

	mu        sync.Mutex
	cond      *sync.Cond
	size      atomic.Int64
	final     atomic.Bool
	failure   error
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
	cmd := &Transcoder{}
	cmd, err = startTranscoderToFile(ctx, opts.FFmpegPath, args, cmd)
	if err != nil {
		os.Remove(tmpPath)
		return nil, err
	}

	estimate := estimateOutputSize(probe, opts)

	t := &transcodeSource{
		tmpPath:   tmpPath,
		cmd:       cmd,
		name:      displayName,
		estimate:  estimate,
		startedAt: time.Now(),
	}
	t.cond = sync.NewCond(&t.mu)

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

// watchSize polls the temp file size every 200 ms and wakes any blocked
// ReadAt callers once new bytes arrive.
func (t *transcodeSource) watchSize(ctx context.Context) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.mu.Lock()
			t.cond.Broadcast()
			t.mu.Unlock()
			return
		case <-ticker.C:
		}
		if t.final.Load() {
			t.mu.Lock()
			t.cond.Broadcast()
			t.mu.Unlock()
			return
		}
		stat, err := os.Stat(t.tmpPath)
		if err != nil {
			continue
		}
		current := stat.Size()
		if current > t.size.Load() {
			t.size.Store(current)
			t.mu.Lock()
			t.cond.Broadcast()
			t.mu.Unlock()
		}
	}
}

// watchExit waits for ffmpeg to exit and locks in the final size.
func (t *transcodeSource) watchExit() {
	err := t.cmd.cmd.Wait()
	if err != nil && !isExpectedExit(err) {
		t.mu.Lock()
		t.failure = fmt.Errorf("ffmpeg exited: %w (%s)", err, t.cmd.Stderr())
		t.mu.Unlock()
	}
	if stat, err := os.Stat(t.tmpPath); err == nil {
		t.size.Store(stat.Size())
	}
	t.final.Store(true)
	t.mu.Lock()
	t.cond.Broadcast()
	t.mu.Unlock()
}

func isExpectedExit(err error) bool {
	// Killed by Close() — pion DC closed, that's fine.
	if err == nil {
		return true
	}
	return false
}

func (t *transcodeSource) ReadAt(p []byte, off int64) (int, error) {
	if t.failure != nil {
		return 0, t.failure
	}
	if int64(len(p)) == 0 {
		return 0, nil
	}
	deadline := time.Now().Add(readBlockTimeout)

	for {
		size := t.size.Load()
		if off+int64(len(p)) <= size || t.final.Load() {
			break
		}
		// Need to wait for ffmpeg to write more.
		t.mu.Lock()
		// Check again under lock to avoid lost wakeup.
		size = t.size.Load()
		if off+int64(len(p)) <= size || t.final.Load() {
			t.mu.Unlock()
			break
		}
		// Wait with timeout via a small sleep loop — sync.Cond doesn't
		// support timed wait, and a goroutine-per-sleep pattern works fine
		// for our scale.
		waited := time.NewTimer(500 * time.Millisecond)
		done := make(chan struct{})
		go func() {
			t.cond.Wait()
			close(done)
		}()
		t.mu.Unlock()
		select {
		case <-done:
		case <-waited.C:
			t.mu.Lock()
			t.cond.Broadcast() // wake the goroutine so it can return
			t.mu.Unlock()
			<-done
		}
		if time.Now().After(deadline) {
			break
		}
	}

	if t.failure != nil {
		return 0, t.failure
	}

	n, err := t.tmpFile.ReadAt(p, off)
	// On growing file ReadAt returns io.EOF when reading past current size.
	// Convert to io.ErrUnexpectedEOF only when we actually exceeded the
	// final size; otherwise return n, nil so the pump sends what we have.
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
	// Keep the original extension stripped — output is always fragmented MP4.
	base := t.name
	if i := lastIndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return base + ".mp4"
}
func (t *transcodeSource) Transcoded() bool { return true }
func (t *transcodeSource) Close() error {
	_ = t.cmd.Close()
	if t.tmpFile != nil {
		_ = t.tmpFile.Close()
	}
	if t.tmpPath != "" {
		_ = os.Remove(t.tmpPath)
	}
	return nil
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

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}
