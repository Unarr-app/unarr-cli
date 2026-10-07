package engine

import "testing"

func TestCopyVideoCodecAllowed(t *testing.T) {
	tests := []struct {
		name     string
		browser  []string
		codec    string
		bitDepth int
		want     bool
	}{
		{"hevc not in [h264] transcodes", []string{"h264"}, "hevc", 8, false},
		{"h264 8-bit in [h264] copies", []string{"h264"}, "h264", 8, true},
		{"h264 unknown depth in [h264] copies", []string{"h264"}, "h264", 0, true},
		{"h264 10-bit transcodes", []string{"h264"}, "h264", 10, false},
		{"h264 10-bit transcodes even when hevc is listed", []string{"h264", "hevc"}, "h264", 10, false},
		{"hevc in [h264,hevc] copies", []string{"h264", "hevc"}, "hevc", 10, true},
		{"h265 spelling is hevc", []string{"H265"}, "hevc", 8, true},
		{"av1 not listed transcodes", []string{"h264", "hevc"}, "av1", 8, false},
		{"unknown source codec transcodes", []string{"h264"}, "", 8, false},
		{"empty list copies (old web)", nil, "hevc", 10, true},
		{"empty list copies even h264 10-bit", []string{}, "h264", 10, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := copyVideoCodecAllowed(tc.browser, tc.codec, tc.bitDepth); got != tc.want {
				t.Errorf("copyVideoCodecAllowed(%v, %q, %d) = %v, want %v", tc.browser, tc.codec, tc.bitDepth, got, tc.want)
			}
		})
	}
}

func TestCopyVideoCodecLabel(t *testing.T) {
	if got := copyVideoCodecLabel("h264", 10); got != "h264 10-bit" {
		t.Errorf("label = %q, want h264 10-bit", got)
	}
	if got := copyVideoCodecLabel("HEVC", 10); got != "hevc" {
		t.Errorf("label = %q, want hevc", got)
	}
}
