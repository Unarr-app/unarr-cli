package engine

import (
	"strings"
	"testing"
)

func TestBuildHLSFFmpegArgsVAAPI(t *testing.T) {
	cfg := HLSSessionConfig{
		SessionID:  "test",
		SourcePath: "/tmp/test.mkv",
		Quality:    "720p",
		AudioIndex: 0,
		Transcode: TranscodeRuntime{
			FFmpegPath:  "/usr/bin/ffmpeg",
			FFprobePath: "/usr/bin/ffprobe",
			HWAccel:     HWAccelVAAPI,
		},
	}
	probe := &StreamProbe{Width: 1920, Height: 1080, DurationSec: 100}
	args := buildHLSFFmpegArgsAt(cfg, probe, "/tmp/tmpdir", 0, 0)
	got := strings.Join(args, " ")

	wants := []string{
		"-hwaccel vaapi",
		"-vaapi_device /dev/dri/renderD128",
		"-c:v h264_vaapi",
		"format=nv12",
		"hwupload",
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "scale_vaapi") {
		t.Errorf("argv unexpectedly contains scale_vaapi (mesa bug): %s", got)
	}
	if strings.Contains(got, "format=yuv420p") {
		t.Errorf("argv contains format=yuv420p (libx264 path) for VAAPI codec: %s", got)
	}
}

func TestBuildHLSFFmpegArgsLibx264NoRegression(t *testing.T) {
	cfg := HLSSessionConfig{
		SessionID:  "test",
		SourcePath: "/tmp/test.mkv",
		Quality:    "720p",
		AudioIndex: 0,
		Transcode: TranscodeRuntime{
			FFmpegPath:  "/usr/bin/ffmpeg",
			FFprobePath: "/usr/bin/ffprobe",
			HWAccel:     HWAccelNone,
		},
	}
	probe := &StreamProbe{Width: 1920, Height: 1080, DurationSec: 100}
	args := buildHLSFFmpegArgsAt(cfg, probe, "/tmp/tmpdir", 0, 0)
	got := strings.Join(args, " ")
	for _, want := range []string{"-c:v libx264", "format=yuv420p", "setparams=colorspace=bt709"} {
		if !strings.Contains(got, want) {
			t.Errorf("libx264 argv missing %q: %s", want, got)
		}
	}
	for _, bad := range []string{"-vaapi_device", "format=nv12", "hwupload"} {
		if strings.Contains(got, bad) {
			t.Errorf("libx264 argv unexpectedly contains %q: %s", bad, got)
		}
	}
}

// TestBuildHLSFFmpegArgsVAAPIDump prints the full argv buildHLSFFmpegArgsAt
// emits for a typical VAAPI session. Mimics the daemon spawn step so the
// operator can verify the ffmpeg command-line shape without booting the
// stack — equivalent to `journalctl --user -u unarr-dev | grep ffmpeg`
// but without waiting for a real player session.
func TestBuildHLSFFmpegArgsVAAPIDump(t *testing.T) {
	cfg := HLSSessionConfig{
		SessionID:  "vaapi-smoke",
		SourcePath: "/mnt/nas/peliculas/sample.mkv",
		Quality:    "720p",
		AudioIndex: -1,
		Transcode: TranscodeRuntime{
			FFmpegPath:  "/usr/bin/ffmpeg",
			FFprobePath: "/usr/bin/ffprobe",
			HWAccel:     HWAccelVAAPI,
		},
	}
	probe := &StreamProbe{
		VideoCodec:  "hevc",
		Width:       3840,
		Height:      2160,
		DurationSec: 5400,
		AudioTracks: []ProbeAudioTrack{{Index: 0, Lang: "en", Codec: "ac3"}},
	}
	args := buildHLSFFmpegArgsAt(cfg, probe, "/tmp/smoke-tmpdir", 0, 0)
	t.Logf("ffmpeg %s", strings.Join(args, " "))
}
