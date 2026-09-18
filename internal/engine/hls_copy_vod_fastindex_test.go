package engine

import (
	"math"
	"testing"
)

func TestValidatedCopySegments(t *testing.T) {
	kfs := []float64{0, 4.004, 8.008, 12.012, 16.016, 20.020}
	starts := validatedCopySegments(kfs, 24)
	if len(starts) != 4 {
		t.Fatalf("bad table: %v", starts)
	}
	for _, cut := range starts[1 : len(starts)-1] {
		found := false
		for _, kf := range kfs {
			found = found || cut == kf
		}
		if !found {
			t.Fatalf("fabricated boundary: %f", cut)
		}
	}
	for _, bad := range [][]float64{nil, {300, 306, 312}, {0, 6, math.NaN()}, {0, 6, math.Inf(1)}, {0, 8, 7}, {0, 6, 6}, {-1, 6}, {0, 24}, {0, 99}} {
		if got := validatedCopySegments(bad, 24); got != nil {
			t.Errorf("accepted invalid %v: %v", bad, got)
		}
	}
	if got := validatedCopySegments([]float64{0, 6}, 3600); got != nil {
		t.Errorf("accepted incomplete window: %v", got)
	}
}
