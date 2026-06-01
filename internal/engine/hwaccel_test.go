package engine

import (
	"strings"
	"testing"
)

func TestHWAccelFFmpegVideoCodec(t *testing.T) {
	cases := []struct {
		hw     HWAccel
		target string
		want   string
	}{
		{HWAccelNone, "h264", "libx264"},
		{HWAccelNone, "hevc", "libx264"},
		{HWAccelNVENC, "h264", "h264_nvenc"},
		{HWAccelNVENC, "hevc", "hevc_nvenc"},
		{HWAccelQSV, "h264", "h264_qsv"},
		{HWAccelQSV, "hevc", "hevc_qsv"},
		{HWAccelVAAPI, "h264", "h264_vaapi"},
		{HWAccelVAAPI, "hevc", "hevc_vaapi"},
		{HWAccelVideoToolbox, "h264", "h264_videotoolbox"},
		{HWAccelVideoToolbox, "hevc", "hevc_videotoolbox"},
	}
	for _, tc := range cases {
		if got := tc.hw.FFmpegVideoCodec(tc.target); got != tc.want {
			t.Errorf("%s.FFmpegVideoCodec(%q) = %q want %q", tc.hw, tc.target, got, tc.want)
		}
	}
}

func TestDetectHWAccelEmptyPathReturnsNone(t *testing.T) {
	ResetHWAccelCache()
	if got := detectHWAccelFresh(t.Context(), ""); got != HWAccelNone {
		t.Errorf("got %s, want %s", got, HWAccelNone)
	}
}

func TestResolveEncoderProfileDefaults(t *testing.T) {
	cases := []struct {
		hw         HWAccel
		configured string
		wantCodec  string
		wantPreset string
		wantHint   string
	}{
		// Empty configured preset → pick latency-biased default per backend.
		// DecodeHwAccel matches the encoder family for HW encoders; libx264 +
		// VideoToolbox have no demuxer hint.
		{HWAccelNone, "", "libx264", "superfast", ""},
		{HWAccelNVENC, "", "h264_nvenc", "p3", "cuda"},
		{HWAccelQSV, "", "h264_qsv", "veryfast", "qsv"},
		// VAAPI: decoder hint set, no preset, no `-hwaccel_output_format vaapi`
		// (so the CPU filter chain can consume the decoded frames).
		{HWAccelVAAPI, "", "h264_vaapi", "", "vaapi"},
		// VideoToolbox has no preset knob — Preset should be "" regardless of input.
		// VideoToolbox uses per-encoder flags, not a demuxer `-hwaccel` hint.
		{HWAccelVideoToolbox, "p4", "h264_videotoolbox", "", ""},
		{HWAccelVideoToolbox, "", "h264_videotoolbox", "", ""},
	}
	for _, tc := range cases {
		got := ResolveEncoderProfile(tc.hw, tc.configured)
		if got.Codec != tc.wantCodec || got.Preset != tc.wantPreset || got.DecodeHwAccel != tc.wantHint {
			t.Errorf("ResolveEncoderProfile(%s, %q) = {codec=%s preset=%s hint=%s}, want {codec=%s preset=%s hint=%s}",
				tc.hw, tc.configured,
				got.Codec, got.Preset, got.DecodeHwAccel,
				tc.wantCodec, tc.wantPreset, tc.wantHint)
		}
	}
}

func TestResolveEncoderProfileHonoursConfiguredPreset(t *testing.T) {
	// Only libx264 honours the configured preset — the libx264 vocabulary
	// (ultrafast…veryslow) doesn't apply to vendor encoders. NVENC has its
	// own p1-p7 scale; QSV uses a different subset; VideoToolbox has no
	// preset knob. Passing a libx264 preset to them would have ffmpeg reject
	// the argv, so ResolveEncoderProfile always falls back to the hardcoded
	// vendor preset for non-libx264 codecs.
	cases := []struct {
		hw         HWAccel
		configured string
		wantPreset string
	}{
		{HWAccelNone, "ultrafast", "ultrafast"}, // libx264 honours
		{HWAccelNone, "medium", "medium"},       // libx264 honours
		{HWAccelNVENC, "p1", "p3"},              // NVENC ignores, sticks to p3
		{HWAccelNVENC, "veryfast", "p3"},        // NVENC ignores libx264 vocab
		{HWAccelQSV, "veryslow", "veryfast"},    // QSV ignores, sticks to veryfast
		{HWAccelVideoToolbox, "veryfast", ""},   // VideoToolbox has no preset
	}
	for _, tc := range cases {
		got := ResolveEncoderProfile(tc.hw, tc.configured)
		if got.Preset != tc.wantPreset {
			t.Errorf("ResolveEncoderProfile(%s, %q).Preset = %q, want %q",
				tc.hw, tc.configured, got.Preset, tc.wantPreset)
		}
	}
}

func TestHWAccelDiagnosticLogLineNone(t *testing.T) {
	d := HWAccelDiagnostic{
		Pick:          HWAccelNone,
		FFmpegPath:    "/usr/local/bin/ffmpeg",
		FFmpegVersion: "ffmpeg version 6.1.1",
		Encoders:      nil,
		Devices:       nil,
	}
	line := d.LogLine()
	wantSubstrings := []string{
		"ffmpeg version 6.1.1",
		"/usr/local/bin/ffmpeg",
		"HW=none",
		"software libx264",
		"no HW encoders compiled in",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(line, want) {
			t.Errorf("expected substring %q in log line; got %q", want, line)
		}
	}
}

func TestHWAccelDiagnosticLogLineNVENCWithDevices(t *testing.T) {
	d := HWAccelDiagnostic{
		Pick:          HWAccelNVENC,
		FFmpegPath:    "/usr/bin/ffmpeg",
		FFmpegVersion: "ffmpeg version 6.0",
		Encoders:      []string{"h264_nvenc", "hevc_nvenc", "h264_qsv"},
		Devices:       []string{"/dev/nvidia0", "nvidia-smi"},
	}
	line := d.LogLine()
	for _, want := range []string{"HW=nvenc", "h264_nvenc", "/dev/nvidia0", "nvidia-smi"} {
		if !strings.Contains(line, want) {
			t.Errorf("expected substring %q in log line; got %q", want, line)
		}
	}
}

func TestHWAccelDiagnosticLogLineSoftwareButEncodersFound(t *testing.T) {
	// Edge case: ffmpeg compiled WITH nvenc but no /dev/nvidia0 (container w/o GPU).
	// LogLine should flag the encoders so the user knows where the gap is.
	d := HWAccelDiagnostic{
		Pick:          HWAccelNone,
		FFmpegPath:    "/usr/bin/ffmpeg",
		FFmpegVersion: "ffmpeg version 6.0",
		Encoders:      []string{"h264_nvenc"},
		Devices:       nil,
	}
	line := d.LogLine()
	for _, want := range []string{"HW=none", "encoders found but no matching device", "h264_nvenc"} {
		if !strings.Contains(line, want) {
			t.Errorf("expected substring %q in log line; got %q", want, line)
		}
	}
}

func TestH264LevelForFrame(t *testing.T) {
	cases := []struct {
		name          string
		width, height int
		want          string
	}{
		// 16:9 must match the height-only helper exactly (no regression).
		{"720p 16:9", 1280, 720, "4.0"},
		{"1080p 16:9", 1920, 1080, "4.1"},
		{"1440p 16:9", 2560, 1440, "5.0"},
		{"2160p 16:9", 3840, 2160, "5.1"},
		// Anamorphic 2.39:1 at 1080 height — the regression: ~2586×1080 = 11016
		// MBs busts level 4.1 (8192 MaxFS); must bump to 5.0.
		{"1080h anamorphic 2.39:1", 2586, 1080, "5.0"},
		// Anamorphic 720 height (1728×720 = 4860 MBs) still fits the 4.0 the
		// height floor already picks for fps headroom.
		{"720h anamorphic 2.4:1", 1728, 720, "4.0"},
		// Source 4K anamorphic (3840×1604) encoded at source: 24240 MBs → 5.1.
		{"4K anamorphic source", 3840, 1604, "5.1"},
		// Width unknown → fall back to the height-only tier.
		{"width unknown", 0, 1080, "4.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := H264LevelForFrame(c.width, c.height); got != c.want {
				t.Errorf("H264LevelForFrame(%d,%d) = %q, want %q", c.width, c.height, got, c.want)
			}
		})
	}
}
