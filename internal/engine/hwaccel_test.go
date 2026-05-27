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
	}{
		// Empty configured preset → pick latency-biased default per backend.
		{HWAccelNone, "", "libx264", "superfast"},
		{HWAccelNVENC, "", "h264_nvenc", "p3"},
		{HWAccelQSV, "", "h264_qsv", "veryfast"},
		// VideoToolbox has no preset knob — Preset should be "" regardless of input.
		{HWAccelVideoToolbox, "p4", "h264_videotoolbox", ""},
		{HWAccelVideoToolbox, "", "h264_videotoolbox", ""},
		// VAAPI codec name resolved correctly; no preset substitution (uses "").
		{HWAccelVAAPI, "", "h264_vaapi", ""},
	}
	for _, tc := range cases {
		got := ResolveEncoderProfile(tc.hw, tc.configured)
		if got.Codec != tc.wantCodec || got.Preset != tc.wantPreset {
			t.Errorf("ResolveEncoderProfile(%s, %q) = {%s, %s}, want {%s, %s}",
				tc.hw, tc.configured, got.Codec, got.Preset, tc.wantCodec, tc.wantPreset)
		}
	}
}

func TestResolveEncoderProfileHonoursConfiguredPreset(t *testing.T) {
	// libx264 / NVENC / QSV all defer to the configured preset when set.
	cases := []struct {
		hw         HWAccel
		configured string
		wantPreset string
	}{
		{HWAccelNone, "ultrafast", "ultrafast"},
		{HWAccelNVENC, "p1", "p1"},
		{HWAccelQSV, "veryslow", "veryslow"},
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

