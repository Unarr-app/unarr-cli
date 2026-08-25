package mediainfo

import "testing"

// TestIsAssMuxerRefusal pins both wordings ffmpeg builds use when the ass muxer
// declines a non-ass stream. CI downloads whatever static build is current, so
// the newer wording reached us first there — as a generic extraction error an
// operator would read as an incident, instead of the informative decline.
func TestIsAssMuxerRefusal(t *testing.T) {
	cases := map[string]bool{
		"[ass @ 0x1] ass muxer supports only codec ass":                           true,
		"[ass @ 0x20588540] Exactly one ASS/SSA stream is needed.":                true,
		"Could not write header (incorrect codec parameters ?): Invalid argument": false,
		"": false,
	}
	for in, want := range cases {
		if got := isAssMuxerRefusal(in); got != want {
			t.Errorf("isAssMuxerRefusal(%q) = %v, want %v", in, got, want)
		}
	}
}
