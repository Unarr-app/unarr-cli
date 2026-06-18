package engine

import (
	"math"
	"strings"
	"testing"
)

func TestCopyVODEligibleCodec(t *testing.T) {
	cases := map[string]bool{
		"h264": true, "avc": true, "avc1": true, "H264": true,
		"hevc": false, "h265": false, "av1": false, "vp9": false, "": false,
	}
	for codec, want := range cases {
		if got := copyVODEligibleCodec(codec); got != want {
			t.Errorf("copyVODEligibleCodec(%q)=%v want %v", codec, got, want)
		}
	}
}

func TestPlanCopySegments(t *testing.T) {
	// Keyframes every 2.002s (the common WEB-DL cadence), 30s duration.
	kfs := []float64{}
	for i := 0; i < 16; i++ {
		kfs = append(kfs, float64(i)*2.002)
	}
	starts := planCopySegments(kfs, 30.0)

	// First boundary is always 0; last is always the true duration.
	if starts[0] != 0 {
		t.Errorf("first start = %v, want 0", starts[0])
	}
	if last := starts[len(starts)-1]; math.Abs(last-30.0) > 1e-9 {
		t.Errorf("last start = %v, want 30.0", last)
	}
	// Strictly increasing.
	for i := 1; i < len(starts); i++ {
		if starts[i] <= starts[i-1] {
			t.Fatalf("starts not strictly increasing at %d: %v", i, starts)
		}
	}
	// Every interior boundary must be a real keyframe (copy can only cut there).
	isKf := func(v float64) bool {
		for _, k := range kfs {
			if math.Abs(k-v) < 1e-6 {
				return true
			}
		}
		return false
	}
	for i := 1; i < len(starts)-1; i++ {
		if !isKf(starts[i]) {
			t.Errorf("interior boundary %v is not a keyframe", starts[i])
		}
	}
	// Each non-final segment must be >= the ~6s target (greedy grouping).
	for i := 0; i+2 < len(starts); i++ {
		if d := starts[i+1] - starts[i]; d < copyVODTargetSec-1e-6 {
			t.Errorf("segment %d duration %v below target %v", i, d, copyVODTargetSec)
		}
	}
}

func TestPlanCopySegmentsShortSource(t *testing.T) {
	// A source shorter than one target segment → a single segment [0,dur].
	starts := planCopySegments([]float64{0}, 3.0)
	if len(starts) != 2 || starts[0] != 0 || math.Abs(starts[1]-3.0) > 1e-9 {
		t.Fatalf("short source plan = %v, want [0 3]", starts)
	}
}

func TestPlanCopySegmentsFoldsTinyTail(t *testing.T) {
	// A keyframe ~0.3s before the end must NOT create a sub-1s final segment.
	kfs := []float64{0, 6.0, 12.0, 17.7}
	starts := planCopySegments(kfs, 18.0)
	for i := 1; i < len(starts); i++ {
		if d := starts[i] - starts[i-1]; d < 1.0 {
			t.Errorf("segment %d duration %v < 1s (tiny tail not folded): %v", i-1, d, starts)
		}
	}
	if last := starts[len(starts)-1]; math.Abs(last-18.0) > 1e-9 {
		t.Errorf("last start = %v, want 18.0", last)
	}
}

func TestRenderVideoPlaylistCopyVOD(t *testing.T) {
	starts := []float64{0, 6.006, 12.012, 18.0}
	m := renderVideoPlaylistCopyVOD(starts)
	for _, want := range []string{
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXT-X-ENDLIST",
		"#EXT-X-INDEPENDENT-SEGMENTS",
		"seg-0.ts", "seg-1.ts", "seg-2.ts",
		"#EXTINF:6.006,",
		"#EXTINF:5.988,", // 18.0 - 12.012
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q\n%s", want, m)
		}
	}
	// MPEG-TS copy-vod carries no fMP4 init.
	if strings.Contains(m, "EXT-X-MAP") || strings.Contains(m, ".m4s") {
		t.Errorf("copy-vod manifest must not reference an fMP4 init / .m4s\n%s", m)
	}
	// 3 segments listed.
	if n := strings.Count(m, "#EXTINF:"); n != 3 {
		t.Errorf("expected 3 segments, got %d", n)
	}
	// TARGETDURATION must be >= longest segment (6.006 → 7).
	if !strings.Contains(m, "#EXT-X-TARGETDURATION:7") {
		t.Errorf("expected TARGETDURATION:7\n%s", m)
	}
}
