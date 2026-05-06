package engine

import "testing"

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
