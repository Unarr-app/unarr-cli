package streaming

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/torrentclaw/unarr/internal/library/mediainfo"
)

// These tests need a real ffmpeg + ffprobe on PATH. They're skipped on
// CI runners that lack them — the unit tests already pin the recipes
// deterministically. Run locally when changing the transcoder pipeline.

func resolveBins(t *testing.T) (string, string) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH — skipping integration test")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not on PATH — skipping integration test")
	}
	return ffmpeg, ffprobe
}

// generateTestVideo synthesises a short MP4 for the transcoder to chew on.
// vcodec/acodec let us exercise both direct-play and transcode branches.
func generateTestVideo(t *testing.T, ffmpeg, dir, vcodec, acodec, container string) string {
	t.Helper()
	out := filepath.Join(dir, "sample."+container)
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=15",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", vcodec,
	}
	// libx265 needs at least one keyframe; 2s @ 15fps is fine.
	if vcodec == "libx265" {
		args = append(args, "-x265-params", "log-level=error")
	}
	args = append(args, "-c:a", acodec, "-shortest", out)
	cmd := exec.Command(ffmpeg, args...)
	if buf, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not synthesise test video (%s/%s/%s): %v\n%s",
			vcodec, acodec, container, err, buf)
	}
	return out
}

// probeOutput uses ffprobe to inspect the (synthesised) transcoder output
// and returns video + audio codec names.
func probeOutput(t *testing.T, ffprobe, path string) (string, string) {
	t.Helper()
	cmd := exec.Command(ffprobe,
		"-hide_banner", "-loglevel", "error",
		"-print_format", "json", "-show_streams", path)
	buf, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var data struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(buf, &data); err != nil {
		t.Fatalf("ffprobe parse: %v", err)
	}
	var v, a string
	for _, s := range data.Streams {
		switch s.CodecType {
		case "video":
			v = s.CodecName
		case "audio":
			a = s.CodecName
		}
	}
	return v, a
}

// TestTranscoder_DirectPlayProducesH264 — H.264 + AAC source → direct play
// → output keeps both codecs, just remuxed to fMP4.
func TestTranscoder_DirectPlayProducesH264(t *testing.T) {
	ffmpeg, ffprobe := resolveBins(t)
	dir := t.TempDir()
	src := generateTestVideo(t, ffmpeg, dir, "libx264", "aac", "mp4")

	tr, err := NewTranscoder(ffmpeg, ffprobe)
	if err != nil {
		t.Fatalf("NewTranscoder: %v", err)
	}

	report, _, err := tr.Analyze(context.Background(), src)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if !report.DirectPlay {
		t.Fatalf("h264+aac sample should be direct-playable, got %+v", report)
	}

	out := filepath.Join(dir, "out.mp4")
	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create out: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := tr.Stream(ctx, src, f, StreamOptions{HW: HWAccelNone}); err != nil {
		f.Close()
		t.Fatalf("Stream: %v", err)
	}
	f.Close()

	v, a := probeOutput(t, ffprobe, out)
	if v != "h264" {
		t.Fatalf("direct-play output video codec = %q want h264", v)
	}
	if a != "aac" {
		t.Fatalf("direct-play output audio codec = %q want aac", a)
	}
}

// TestTranscoder_TranscodeHEVCToH264 — HEVC source → transcode →
// output is H.264 + AAC ready for the browser.
func TestTranscoder_TranscodeHEVCToH264(t *testing.T) {
	ffmpeg, ffprobe := resolveBins(t)
	dir := t.TempDir()

	// Verify libx265 available; some Alpine builds disable it.
	if !encoderAvailable(context.Background(), ffmpeg, "libx265") {
		t.Skip("ffmpeg lacks libx265 — skipping HEVC transcode integration")
	}
	src := generateTestVideo(t, ffmpeg, dir, "libx265", "ac3", "mkv")

	tr, err := NewTranscoder(ffmpeg, ffprobe)
	if err != nil {
		t.Fatalf("NewTranscoder: %v", err)
	}
	report, _, err := tr.Analyze(context.Background(), src)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if report.DirectPlay {
		t.Fatalf("hevc+ac3 sample must NOT be direct-playable")
	}

	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := tr.Stream(ctx, src, &buf, StreamOptions{Quality: "480p", HW: HWAccelNone}); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	out := filepath.Join(dir, "transcoded.mp4")
	if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("persist transcode: %v", err)
	}

	v, a := probeOutput(t, ffprobe, out)
	if v != "h264" {
		t.Fatalf("transcoded video codec = %q want h264", v)
	}
	if a != "aac" {
		t.Fatalf("transcoded audio codec = %q want aac", a)
	}
}

// TestTranscoder_AnalyzeReportsRealMediaInfo validates that the Transcoder
// returns a usable MediaInfo on top of the report — the API handler will
// surface duration / resolution to the player UI.
func TestTranscoder_AnalyzeReportsRealMediaInfo(t *testing.T) {
	ffmpeg, ffprobe := resolveBins(t)
	dir := t.TempDir()
	src := generateTestVideo(t, ffmpeg, dir, "libx264", "aac", "mp4")

	tr, err := NewTranscoder(ffmpeg, ffprobe)
	if err != nil {
		t.Fatalf("NewTranscoder: %v", err)
	}
	_, info, err := tr.Analyze(context.Background(), src)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if info == nil || info.Video == nil {
		t.Fatalf("missing parsed mediainfo: %+v", info)
	}
	if info.Video.Width != 320 || info.Video.Height != 240 {
		t.Errorf("dimensions = %dx%d want 320x240", info.Video.Width, info.Video.Height)
	}
	if info.Video.Duration < 1.5 || info.Video.Duration > 2.5 {
		t.Errorf("duration ~2s expected, got %v", info.Video.Duration)
	}
	// Ensure the package types line up with mediainfo's exported model.
	_ = mediainfo.MediaInfo{}
}
