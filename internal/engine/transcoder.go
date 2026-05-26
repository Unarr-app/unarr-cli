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

// TranscodeOpts steers how Transcoder builds its ffmpeg command line.
//
//   - Output: fragmented MP4 chunked into HLS segments by the muxer.
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
	SourceHeight int    // probed source height — used to derive a sane H.264 level
	StartSeconds float64
	FFmpegPath   string
}

// Transcoder wraps a long-running ffmpeg child process whose stdout streams
// fragmented MP4 bytes; the HLS muxer slices them into segments served over HTTP.
//
// One Transcoder == one playback position. A seek beyond the buffered window
// requires Close()ing this transcoder and starting a new one with a higher
// StartSeconds (handled by the HLS session at ffmpeg start time).
//
// A single internal goroutine owns cmd.Wait() — never call cmd.Wait()
// directly from outside (os/exec forbids concurrent Wait callers). Use
// Done() / WaitErr() instead.
type Transcoder struct {
	cmd *exec.Cmd
	out io.ReadCloser

	mu     sync.Mutex
	closed bool
	stderr strings.Builder

	done    chan struct{} // closed once cmd.Wait returns; nil if cmd never started
	waitErr error         // populated before done is closed; read-only after
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
	t.startWaitGoroutine()
	return t, nil
}

// startTranscoderToFile spawns ffmpeg with a pre-built argv where the last
// argument is an output file path (instead of pipe:1). Used by streamSource
// when we want random-access reads against a growing temp file rather than
// sequential pipe consumption.
func startTranscoderToFile(ctx context.Context, ffmpegPath string, args []string, t *Transcoder) (*Transcoder, error) {
	if ffmpegPath == "" {
		return nil, fmt.Errorf("transcoder: empty ffmpeg path")
	}
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	if t == nil {
		t = &Transcoder{}
	}
	t.cmd = cmd
	cmd.Stderr = &errWriter{t: t}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("transcoder: start ffmpeg: %w", err)
	}
	t.startWaitGoroutine()
	return t, nil
}

// startWaitGoroutine launches the single goroutine that owns cmd.Wait().
// Idempotent — protected by sync.Once-via-nil-check on done.
func (t *Transcoder) startWaitGoroutine() {
	if t.done != nil {
		return
	}
	t.done = make(chan struct{})
	go func() {
		t.waitErr = t.cmd.Wait()
		close(t.done)
	}()
}

// Done returns a channel that closes when ffmpeg exits. Returns nil for a
// Transcoder whose cmd never started.
func (t *Transcoder) Done() <-chan struct{} { return t.done }

// WaitErr blocks until ffmpeg exits and returns the wait error. Safe to
// call concurrently from multiple goroutines.
func (t *Transcoder) WaitErr() error {
	if t.done == nil {
		return nil
	}
	<-t.done
	return t.waitErr
}

// Read implements io.Reader.
func (t *Transcoder) Read(p []byte) (int, error) { return t.out.Read(p) }

// Close kills the child process if still running and waits up to 2s for exit.
// IsClosing reports true after Close has been invoked — used by streamSource
// to distinguish a kill-by-Close from a genuine ffmpeg crash.
func (t *Transcoder) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	// out is nil for the file-output flow (startTranscoderToFile) — that
	// pipeline writes directly to a temp file via -i ... output_path so we
	// never wired a stdout pipe. Only close when present.
	if t.out != nil {
		_ = t.out.Close()
	}
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	if t.done == nil {
		return nil
	}
	select {
	case <-t.done:
	case <-time.After(2 * time.Second):
		// Process refused to die — leak it; the OS will clean up on exit.
	}
	return nil
}

// IsClosing reports whether Close has been invoked. Cheap atomic-ish check
// for callers that want to distinguish a kill-by-Close exit from a real
// ffmpeg failure when reading WaitErr.
func (t *Transcoder) IsClosing() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
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
	// -y: overwrite output without asking (the file-output flow uses an
	// already-created tmp file from os.CreateTemp, so the default "do you
	// want to overwrite?" prompt would deadlock on stdin and ffmpeg dies
	// before producing a single byte). Pipe flow doesn't need it but it's
	// harmless there.
	args := []string{"-y", "-hide_banner", "-loglevel", "warning"}

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
		// Force the broadest browser-compatible h264 profile. `high` (libx264
		// default) makes Chrome try its hardware decoder path first, which
		// can fail with "VaapiWrapper: failed initializing" on Linux boxes
		// where VA-API isn't fully wired up. `main` keeps a clean software
		// decode fallback on every desktop + mobile platform.
		//
		// Level is derived from the actual output height — a fixed "4.0"
		// silently rejects 4K and 1440p sources at the libx264 macroblock
		// limits and produces unplayable streams. opts.MaxHeight is the
		// downscale cap when set; falling through means "encode at source".
		levelHeight := opts.MaxHeight
		if levelHeight == 0 || (opts.SourceHeight > 0 && opts.SourceHeight < levelHeight) {
			levelHeight = opts.SourceHeight
		}
		args = append(args, "-profile:v", "main", "-level:v", H264LevelForHeight(levelHeight))
		args = append(args, "-b:v", coalesce(opts.VideoBitrate, "5M"))
		// Filter chain:
		//   1. scale (optional) — cap height + force even width.
		//   2. format=yuv420p — drop 10-bit + reset pix_fmt to 8-bit before
		//      libx264 (which refuses 10-bit unless built with --bit-depth=10).
		//   3. setparams — REWRITE the color metadata in the output stream's
		//      VUI/SEI without touching pixels. This is what makes HDR HEVC
		//      sources (color_primaries=bt2020, color_transfer=arib-std-b67)
		//      decodeable in browsers that reject anything but Rec.709. We
		//      can't actually tonemap without libzimg/zscale (most ffmpeg
		//      builds — including ours — ship without it), so colours look
		//      desaturated on HDR sources, but the file plays. SDR sources
		//      already match these params and are unaffected.
		var filterChain string
		if opts.MaxHeight > 0 {
			filterChain = fmt.Sprintf(
				"scale=-2:%d:force_original_aspect_ratio=decrease,format=yuv420p,setparams=colorspace=bt709:color_trc=bt709:color_primaries=bt709:range=tv",
				opts.MaxHeight,
			)
		} else {
			filterChain = "format=yuv420p,setparams=colorspace=bt709:color_trc=bt709:color_primaries=bt709:range=tv"
		}
		args = append(args, "-vf", filterChain)
		// Force AAC-LC stereo 48 kHz so the hls.js demuxer accepts the moov.
		// 5.1 / 7.1 source streams produce a moov shape the demuxer refuses
		// to parse, so always downmix to stereo + resample to 48 kHz here.
		args = append(args,
			"-c:a", "aac",
			"-b:a", coalesce(opts.AudioBitrate, "192k"),
			"-ar", "48000",
			"-ac", "2",
		)
	}

	// Common output flags — fragmented MP4 to a single pipe.
	//
	//   * empty_moov + default_base_moof: header-only init segment up front
	//     so the demuxer can start decoding before the file is finished.
	//   * frag_duration=1s: cap each moof+mdat at ~1 second of media.
	//     Without it ffmpeg only splits at keyframes; a high-bitrate 1080p
	//     stream produces 8 MiB+ mdat boxes that delay the first fragment
	//     until the whole mdat lands and playback never starts.
	//   * negative_cts_offsets: lets b-frames carry the right pts/dts so
	//     decoders don't reset the playhead to 0 every fragment.
	args = append(args,
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof+negative_cts_offsets",
		"-frag_duration", "1000000",
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
