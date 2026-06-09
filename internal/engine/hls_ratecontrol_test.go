package engine

import (
	"strings"
	"testing"
)

func TestDoubleBitrate(t *testing.T) {
	cases := map[string]string{
		"6000k":   "12000k",
		"25000k":  "50000k",
		"1500k":   "3000k",
		"5M":      "10M",
		"1.5M":    "3M",
		"2.5m":    "5m",
		"800000":  "1600000",
		"":        "",
		"garbage": "garbage", // unparseable → unchanged (1× bufsize fallback)
		"-5M":     "-5M",     // non-positive → unchanged
	}
	for in, want := range cases {
		if got := doubleBitrate(in); got != want {
			t.Errorf("doubleBitrate(%q) = %q, want %q", in, got, want)
		}
	}
}

// segmentIdxForTime must be the exact inverse of segmentStartSec so the
// resume-aware first spawn (HLSSessionConfig.StartSec) lands on the same
// segment the player's hls.js startPosition will request.
func TestSegmentIdxForTime(t *testing.T) {
	cases := map[float64]int{
		0:      0,
		-3:     0,
		0.5:    0,
		1.99:   0,
		2:      1,
		3.9:    1,
		60:     30,
		3599.9: 1799,
	}
	for sec, want := range cases {
		if got := segmentIdxForTime(sec); got != want {
			t.Errorf("segmentIdxForTime(%v) = %d, want %d", sec, got, want)
		}
	}
	// Round-trip: the start time of the segment we resolve must never be
	// AFTER the requested position (the player would miss its first frames).
	for _, sec := range []float64{0, 1, 2, 7.3, 119.9, 4321} {
		idx := segmentIdxForTime(sec)
		if start := segmentStartSec(idx); start > sec {
			t.Errorf("segmentStartSec(segmentIdxForTime(%v)) = %v > %v", sec, start, sec)
		}
	}
}

// Capped constant-quality rate control: libx264 gets -crf (no -b:v), NVENC
// gets -cq with -b:v 0, both keep -maxrate at the level-coherent cap and a
// 2× -bufsize. VAAPI (and the other vendor encoders) keep the proven
// fixed-bitrate triple untouched.
func TestBuildHLSFFmpegArgsRateControl(t *testing.T) {
	probe := &StreamProbe{Width: 1920, Height: 1080, DurationSec: 100}
	base := HLSSessionConfig{
		SessionID:  "test",
		SourcePath: "/media/Movie.mkv",
		Quality:    "1080p",
		Transcode: TranscodeRuntime{
			FFmpegPath:  "/usr/bin/ffmpeg",
			FFprobePath: "/usr/bin/ffprobe",
		},
	}

	t.Run("libx264 capped CRF", func(t *testing.T) {
		cfg := base
		cfg.Transcode.HWAccel = HWAccelNone
		got := strings.Join(buildHLSFFmpegArgsAt(cfg, probe, "/tmp/tmpdir", 0, 0), " ")
		for _, want := range []string{"-crf 23", "-maxrate 6000k", "-bufsize 12000k"} {
			if !strings.Contains(got, want) {
				t.Errorf("libx264 argv missing %q\n%s", want, got)
			}
		}
		if strings.Contains(got, "-b:v 6000k") {
			t.Errorf("libx264 argv must not carry -b:v alongside -crf\n%s", got)
		}
	})

	t.Run("nvenc constant-quality VBR", func(t *testing.T) {
		cfg := base
		cfg.Transcode.HWAccel = HWAccelNVENC
		got := strings.Join(buildHLSFFmpegArgsAt(cfg, probe, "/tmp/tmpdir", 0, 0), " ")
		for _, want := range []string{"-rc vbr", "-cq 23", "-b:v 0", "-maxrate 6000k", "-bufsize 12000k"} {
			if !strings.Contains(got, want) {
				t.Errorf("nvenc argv missing %q\n%s", want, got)
			}
		}
	})

	t.Run("vaapi keeps fixed-bitrate triple", func(t *testing.T) {
		cfg := base
		cfg.Transcode.HWAccel = HWAccelVAAPI
		got := strings.Join(buildHLSFFmpegArgsAt(cfg, probe, "/tmp/tmpdir", 0, 0), " ")
		for _, want := range []string{"-b:v 6000k", "-maxrate 6000k", "-bufsize 6000k"} {
			if !strings.Contains(got, want) {
				t.Errorf("vaapi argv missing %q\n%s", want, got)
			}
		}
		if strings.Contains(got, "-crf") || strings.Contains(got, "-cq") {
			t.Errorf("vaapi argv must not carry constant-quality flags\n%s", got)
		}
	})
}
