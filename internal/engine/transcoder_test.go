package engine

import (
	"strings"
	"testing"
)

func sliceContains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func sliceContainsPair(args []string, key, val string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && args[i+1] == val {
			return true
		}
	}
	return false
}

func TestBuildFFmpegArgsPassthroughCopy(t *testing.T) {
	args := buildFFmpegArgs("/tmp/movie.mp4", TranscodeOpts{
		Action:     ActionPassthrough,
		HWAccel:    HWAccelNone,
		FFmpegPath: "ffmpeg",
	})
	if !sliceContainsPair(args, "-c:v", "copy") {
		t.Errorf("passthrough should keep -c:v copy. args=%v", args)
	}
	if !sliceContainsPair(args, "-c:a", "copy") {
		t.Error("passthrough should keep -c:a copy")
	}
	if !sliceContainsPair(args, "-f", "mp4") {
		t.Error("output container must be mp4")
	}
	movflags := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-movflags" {
			movflags = args[i+1]
		}
	}
	if !strings.Contains(movflags, "frag_keyframe") {
		t.Errorf("movflags must include frag_keyframe, got %q", movflags)
	}
}

func TestBuildFFmpegArgsRemuxAudio(t *testing.T) {
	args := buildFFmpegArgs("/tmp/movie.mkv", TranscodeOpts{
		Action:       ActionRemuxAudio,
		AudioBitrate: "256k",
		FFmpegPath:   "ffmpeg",
	})
	if !sliceContainsPair(args, "-c:v", "copy") {
		t.Error("remux-audio keeps video copy")
	}
	if !sliceContainsPair(args, "-c:a", "aac") {
		t.Error("remux-audio must transcode audio to aac")
	}
	if !sliceContainsPair(args, "-b:a", "256k") {
		t.Error("audio bitrate override not honored")
	}
}

func TestBuildFFmpegArgsTranscodeVideoSoftware(t *testing.T) {
	args := buildFFmpegArgs("/tmp/movie.mkv", TranscodeOpts{
		Action:       ActionTranscodeVideo,
		HWAccel:      HWAccelNone,
		Preset:       "fast",
		VideoBitrate: "6M",
		FFmpegPath:   "ffmpeg",
	})
	if !sliceContainsPair(args, "-c:v", "libx264") {
		t.Error("software fallback must use libx264")
	}
	if !sliceContainsPair(args, "-preset", "fast") {
		t.Error("custom preset not honored")
	}
	if !sliceContainsPair(args, "-b:v", "6M") {
		t.Error("video bitrate not honored")
	}
}

func TestBuildFFmpegArgsTranscodeVideoNVENC(t *testing.T) {
	args := buildFFmpegArgs("/tmp/movie.mkv", TranscodeOpts{
		Action:     ActionTranscodeVideo,
		HWAccel:    HWAccelNVENC,
		FFmpegPath: "ffmpeg",
	})
	if !sliceContainsPair(args, "-hwaccel", "cuda") {
		t.Error("NVENC must request -hwaccel cuda")
	}
	if !sliceContainsPair(args, "-c:v", "h264_nvenc") {
		t.Error("NVENC must use h264_nvenc encoder")
	}
	if sliceContains(args, "-preset") {
		// HW encoders ignore software preset; we should NOT pass it.
		t.Error("HW encoder path should not include -preset")
	}
}

func TestBuildFFmpegArgsAddsStartSeek(t *testing.T) {
	args := buildFFmpegArgs("/tmp/movie.mp4", TranscodeOpts{
		Action:       ActionPassthrough,
		StartSeconds: 90.5,
		FFmpegPath:   "ffmpeg",
	})
	idxSs, idxIn := -1, -1
	for i, a := range args {
		if a == "-ss" {
			idxSs = i
		}
		if a == "-i" {
			idxIn = i
		}
	}
	if idxSs < 0 {
		t.Fatal("missing -ss flag")
	}
	if idxIn < 0 {
		t.Fatal("missing -i flag")
	}
	if idxSs >= idxIn {
		t.Errorf("expected -ss BEFORE -i for fast seek; got -ss@%d -i@%d", idxSs, idxIn)
	}
	if args[idxSs+1] != "90.500" {
		t.Errorf("expected seek 90.500s, got %q", args[idxSs+1])
	}
}

func TestTranscoderZeroValueLifecycle(t *testing.T) {
	var tr Transcoder
	if tr.IsClosing() {
		t.Errorf("zero-value Transcoder should not report IsClosing")
	}
	if tr.Stderr() != "" {
		t.Errorf("zero-value Stderr should be empty")
	}
	if err := tr.WaitErr(); err != nil {
		t.Errorf("WaitErr without started cmd should be nil, got %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("Close without started cmd should be nil, got %v", err)
	}
	// Second Close is idempotent and must remain nil.
	if err := tr.Close(); err != nil {
		t.Errorf("repeat Close should be nil, got %v", err)
	}
	if !tr.IsClosing() {
		t.Errorf("after Close, IsClosing should be true")
	}
	if tr.Done() != nil {
		t.Errorf("Done() should be nil for never-started Transcoder")
	}
}

func TestErrWriterCapturesStderr(t *testing.T) {
	tr := &Transcoder{}
	w := &errWriter{t: tr}
	n, err := w.Write([]byte("ffmpeg failed: bad codec"))
	if err != nil || n != 24 {
		t.Errorf("Write returned (%d,%v)", n, err)
	}
	if got := tr.Stderr(); got != "ffmpeg failed: bad codec" {
		t.Errorf("Stderr captured %q", got)
	}
}

func TestErrWriterCapsBuffer(t *testing.T) {
	tr := &Transcoder{}
	w := &errWriter{t: tr}
	// Write a chunk under the cap, then a huge chunk: total should stop growing past 64KB.
	w.Write(make([]byte, 32*1024)) //nolint:errcheck
	w.Write(make([]byte, 32*1024)) //nolint:errcheck
	w.Write(make([]byte, 32*1024)) //nolint:errcheck
	if got := len(tr.Stderr()); got > 64*1024 {
		t.Errorf("stderr exceeded 64KB cap: %d bytes", got)
	}
}

func TestCoalesce(t *testing.T) {
	if got := coalesce("", "fallback"); got != "fallback" {
		t.Errorf("empty -> fallback, got %q", got)
	}
	if got := coalesce("value", "fallback"); got != "value" {
		t.Errorf("non-empty -> value, got %q", got)
	}
}

func TestBuildFFmpegArgsDownscale(t *testing.T) {
	args := buildFFmpegArgs("/tmp/movie.mkv", TranscodeOpts{
		Action:     ActionTranscodeVideo,
		HWAccel:    HWAccelNone,
		MaxHeight:  720,
		FFmpegPath: "ffmpeg",
	})
	hasVF := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-vf" && strings.Contains(args[i+1], "720") {
			hasVF = true
		}
	}
	if !hasVF {
		t.Errorf("expected -vf scale containing 720; args=%v", args)
	}
}
