package streaming

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/torrentclaw/unarr/internal/library/mediainfo"
)

// Transcoder owns the resolved ffmpeg / ffprobe binaries plus the
// detected hardware accelerator. One per process; safe for concurrent use.
type Transcoder struct {
	ffmpegPath  string
	ffprobePath string

	hwOnce sync.Once
	hw     HWAccel
}

// NewTranscoder constructs a Transcoder from explicit binary paths.
// Both must be non-empty; resolve them upstream via
// mediainfo.ResolveFFmpeg / ResolveFFprobe.
func NewTranscoder(ffmpegPath, ffprobePath string) (*Transcoder, error) {
	if ffmpegPath == "" {
		return nil, errors.New("streaming: ffmpeg path is required")
	}
	if ffprobePath == "" {
		return nil, errors.New("streaming: ffprobe path is required")
	}
	return &Transcoder{
		ffmpegPath:  ffmpegPath,
		ffprobePath: ffprobePath,
	}, nil
}

// HWAccel returns the cached / detected hardware accelerator. First call
// runs `ffmpeg -encoders`; subsequent calls reuse the result.
func (t *Transcoder) HWAccel(ctx context.Context) HWAccel {
	t.hwOnce.Do(func() {
		t.hw = DetectHWAccel(ctx, t.ffmpegPath)
	})
	return t.hw
}

// Analyze runs ffprobe on the input file and returns a compatibility
// report so the caller can decide direct play vs transcode.
func (t *Transcoder) Analyze(ctx context.Context, inputPath string) (CompatibilityReport, *mediainfo.MediaInfo, error) {
	info, err := mediainfo.ExtractMediaInfo(ctx, t.ffprobePath, inputPath)
	if err != nil {
		return CompatibilityReport{}, nil, fmt.Errorf("streaming: ffprobe failed: %w", err)
	}
	return AnalyzeCompatibility(info), info, nil
}

// Stream runs ffmpeg with the right recipe for the given file + options
// and writes fragmented MP4 to dst. Blocks until ffmpeg exits or the
// context is cancelled. If ffmpeg's stderr captures something useful, it's
// included in the returned error.
func (t *Transcoder) Stream(ctx context.Context, inputPath string, dst io.Writer, opts StreamOptions) error {
	report, _, err := t.Analyze(ctx, inputPath)
	if err != nil {
		return err
	}
	return t.StreamWithReport(ctx, inputPath, dst, opts, report)
}

// StreamWithReport is the lower-level entry point — accepts a
// pre-computed CompatibilityReport so the API handler can inspect the
// decision before kicking off a transcode (useful for billing /
// telemetry / quality-fallback policies).
func (t *Transcoder) StreamWithReport(
	ctx context.Context,
	inputPath string,
	dst io.Writer,
	opts StreamOptions,
	report CompatibilityReport,
) error {
	if opts.HW == HWAccelUnset {
		opts.HW = t.HWAccel(ctx)
	}

	args := BuildFFmpegArgs(inputPath, report, opts)
	cmd := exec.CommandContext(ctx, t.ffmpegPath, args...)
	cmd.Stdout = dst

	stderrBuf := newCappedBuffer(8 * 1024) // last 8 KiB is plenty for diagnosing
	cmd.Stderr = stderrBuf

	if err := cmd.Run(); err != nil {
		// Cancellation looks like an exec error too; surface the cause
		// so callers don't blame ffmpeg for client disconnects.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("streaming: ffmpeg exited: %w (stderr tail: %s)", err, stderrBuf.String())
	}
	return nil
}

// cappedBuffer is an io.Writer that keeps only the last `cap` bytes
// written. Used to capture ffmpeg's tail stderr for error reporting
// without unbounded memory growth on long transcodes.
type cappedBuffer struct {
	buf []byte
	cap int
}

func newCappedBuffer(cap int) *cappedBuffer {
	return &cappedBuffer{cap: cap}
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if len(p) >= c.cap {
		c.buf = append(c.buf[:0], p[len(p)-c.cap:]...)
		return len(p), nil
	}
	if len(c.buf)+len(p) > c.cap {
		drop := len(c.buf) + len(p) - c.cap
		c.buf = c.buf[drop:]
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	return string(c.buf)
}
