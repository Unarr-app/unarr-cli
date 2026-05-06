package engine

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TranscodeOpts steers how Transcoder builds its ffmpeg command line. Defaults
// match the project's plan/clever-weaving-dove.md (Fase 2.5):
//
//   - Output: fragmented MP4 readable by browser <video> via MSE-less Range.
//   - Audio: AAC stereo @ 192kbps unless source already AAC (then -c:a copy).
//   - Video: copy when h264 8-bit; otherwise transcode to h264 with HW encode
//     when available, software fallback at "veryfast" preset.
type TranscodeOpts struct {
	Action       TranscodeAction
	HWAccel      HWAccel
	Preset       string // "veryfast" / "fast" / "medium"
	VideoBitrate string // e.g. "5M"
	AudioBitrate string // e.g. "192k"
	MaxHeight    int    // optional downscale cap (e.g. 720)
	StartSeconds float64
	FFmpegPath   string
}

// Transcoder wraps a long-running ffmpeg child process whose stdout streams
// fragmented MP4 bytes for the WebRTC pump to forward to the browser.
//
// One Transcoder == one playback position. A seek beyond the buffered window
// requires Close()ing this transcoder and starting a new one with a higher
// StartSeconds (handled in webrtc_stream.go).
type Transcoder struct {
	cmd *exec.Cmd
	out io.ReadCloser

	mu     sync.Mutex
	closed bool
	stderr strings.Builder
}

// NewTranscoder spawns ffmpeg and returns a Transcoder whose Read() yields
// fragmented MP4 bytes from stdin. Callers MUST call Close() when done.
func NewTranscoder(ctx context.Context, filePath string, opts TranscodeOpts) (*Transcoder, error) {
	if opts.FFmpegPath == "" {
		return nil, fmt.Errorf("transcoder: empty ffmpeg path")
	}
	args := buildFFmpegArgs(filePath, opts)
	cmd := exec.CommandContext(ctx, opts.FFmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("transcoder: stdout pipe: %w", err)
	}
	t := &Transcoder{cmd: cmd, out: stdout}
	cmd.Stderr = &errWriter{t: t}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("transcoder: start ffmpeg: %w", err)
	}
	return t, nil
}

// Read implements io.Reader.
func (t *Transcoder) Read(p []byte) (int, error) { return t.out.Read(p) }

// Close kills the child process if still running and waits up to 2s for exit.
func (t *Transcoder) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	_ = t.out.Close()
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	done := make(chan error, 1)
	go func() { done <- t.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// Process refused to die — leak it; the OS will clean up on exit.
	}
	return nil
}

// Stderr returns the accumulated ffmpeg stderr so far. Useful for surfacing
// failure reasons in logs after Close().
func (t *Transcoder) Stderr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stderr.String()
}

// errWriter funnels ffmpeg stderr into the Transcoder buffer so it can be
// inspected post-mortem. Capped so a misbehaving ffmpeg can't grow memory.
type errWriter struct{ t *Transcoder }

func (w *errWriter) Write(p []byte) (int, error) {
	w.t.mu.Lock()
	defer w.t.mu.Unlock()
	const maxBuf = 64 * 1024
	if w.t.stderr.Len() < maxBuf {
		w.t.stderr.Write(p)
	}
	return len(p), nil
}

// buildFFmpegArgs assembles the command line for the requested action.
// Exposed package-level so tests can lock the flag matrix independently of
// process spawning.
func buildFFmpegArgs(filePath string, opts TranscodeOpts) []string {
	args := []string{"-hide_banner", "-loglevel", "warning"}

	// Seek BEFORE input (-ss before -i) for fast keyframe-aligned start.
	if opts.StartSeconds > 0 {
		args = append(args, "-ss", strconv.FormatFloat(opts.StartSeconds, 'f', 3, 64))
	}

	// HW accel hint on the demuxer side improves throughput for HEVC inputs
	// even when we end up encoding in software. Skip on macOS (videotoolbox
	// uses a different flag shape).
	switch opts.HWAccel {
	case HWAccelNVENC:
		args = append(args, "-hwaccel", "cuda")
	case HWAccelQSV:
		args = append(args, "-hwaccel", "qsv")
	case HWAccelVAAPI:
		args = append(args, "-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi")
	case HWAccelNone, HWAccelVideoToolbox:
		// No demuxer-side hint: software decode (None) or per-encoder flags
		// already applied separately by FFmpegVideoCodec (VideoToolbox).
	}

	args = append(args, "-i", filePath)

	switch opts.Action {
	case ActionPassthrough, ActionRemux:
		args = append(args, "-c:v", "copy", "-c:a", "copy")
	case ActionRemuxAudio:
		args = append(args, "-c:v", "copy", "-c:a", "aac", "-b:a", coalesce(opts.AudioBitrate, "192k"))
	case ActionTranscodeVideo:
		videoCodec := opts.HWAccel.FFmpegVideoCodec("h264")
		args = append(args, "-c:v", videoCodec)
		if videoCodec == "libx264" {
			args = append(args, "-preset", coalesce(opts.Preset, "veryfast"))
		}
		args = append(args, "-b:v", coalesce(opts.VideoBitrate, "5M"))
		if opts.MaxHeight > 0 {
			args = append(args,
				"-vf",
				fmt.Sprintf("scale='min(iw,iw*%d/ih)':'min(ih,%d)'", opts.MaxHeight, opts.MaxHeight),
			)
		}
		args = append(args, "-c:a", "aac", "-b:a", coalesce(opts.AudioBitrate, "192k"))
	}

	// Common output flags — fragmented MP4 to a single pipe.
	args = append(args,
		"-movflags", "frag_keyframe+empty_moov+default_base_moof+faststart",
		"-f", "mp4",
		"pipe:1",
	)
	return args
}

func coalesce(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
