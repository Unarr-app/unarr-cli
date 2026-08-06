package engine

import (
	"math"
	"testing"
)

// planWindowedCopySegments must produce a table with the same invariants
// planCopySegments guarantees — starts[0]==0, strictly increasing, final element
// ==duration — while being keyframe-exact inside the indexed window. A violation
// here corrupts the playlist: EXTINF values stop matching the segments, and the
// per-segment `-ss`/`-to` pair can invert.

func checkTableInvariants(t *testing.T, starts []float64, duration float64) {
	t.Helper()
	if len(starts) < 2 {
		t.Fatalf("table too short: %v", starts)
	}
	if starts[0] != 0 {
		t.Errorf("starts[0] = %v, want 0", starts[0])
	}
	if last := starts[len(starts)-1]; math.Abs(last-duration) > 0.001 {
		t.Errorf("final boundary = %v, want duration %v", last, duration)
	}
	for i := 1; i < len(starts); i++ {
		if starts[i] <= starts[i-1] {
			t.Errorf("not strictly increasing at %d: %v <= %v", i, starts[i], starts[i-1])
		}
	}
}

func TestPlanWindowedCopySegmentsInvariants(t *testing.T) {
	cases := []struct {
		name     string
		window   []float64
		duration float64
	}{
		{"window mid-file", []float64{3543.2, 3549.9, 3556.1, 3562.7, 3570.0, 3577.4}, 8242},
		{"window at file start", []float64{0, 6.1, 12.4, 18.9, 25.2}, 600},
		{"window near the end", []float64{580.5, 587.2, 593.8}, 600},
		{"single keyframe", []float64{3000}, 6000},
		{"dense keyframes (sub-target spacing)", []float64{100, 101, 102, 103, 104, 105, 106}, 500},
		{"sparse keyframes (far apart)", []float64{100, 400, 900}, 1200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			starts := planWindowedCopySegments(tc.window, tc.duration)
			if starts == nil {
				t.Fatal("planning returned nil")
			}
			checkTableInvariants(t, starts, tc.duration)
		})
	}
}

// Every keyframe the window reported that is far enough from its neighbours must
// actually appear as a boundary — that is the whole point of indexing the window.
func TestPlanWindowedCopySegmentsUsesRealKeyframes(t *testing.T) {
	window := []float64{3543.2, 3549.9, 3556.1, 3562.7, 3570.0, 3577.4}
	starts := planWindowedCopySegments(window, 8242)

	present := make(map[float64]bool, len(starts))
	for _, s := range starts {
		present[s] = true
	}
	// Spacing here is ~6.5 s, just over copyVODTargetSec, so each keyframe opens
	// its own segment.
	for _, kf := range window {
		if !present[kf] {
			t.Errorf("keyframe %.3f is not a segment boundary; table=%v", kf, starts)
		}
	}
}

// A boundary must never land past the duration, and a sub-second tail must be
// folded rather than listed as a near-empty final segment.
func TestPlanWindowedCopySegmentsNoTailSliver(t *testing.T) {
	// Window ends 0.4 s before duration → the tail must fold into the previous
	// segment instead of becoming its own.
	starts := planWindowedCopySegments([]float64{100, 106.2, 112.5}, 112.9)
	checkTableInvariants(t, starts, 112.9)
	for i, s := range starts {
		if s > 112.9+0.001 {
			t.Errorf("boundary %d (%v) exceeds duration", i, s)
		}
	}
	for i := 1; i < len(starts); i++ {
		if d := starts[i] - starts[i-1]; d < 1.0 {
			t.Errorf("segment %d is a %.3fs sliver", i-1, d)
		}
	}
}

func TestPlanWindowedCopySegmentsRejectsUnusableInput(t *testing.T) {
	if got := planWindowedCopySegments(nil, 600); got != nil {
		t.Errorf("nil window → %v, want nil", got)
	}
	if got := planWindowedCopySegments([]float64{}, 600); got != nil {
		t.Errorf("empty window → %v, want nil", got)
	}
	if got := planWindowedCopySegments([]float64{100}, 0); got != nil {
		t.Errorf("zero duration → %v, want nil", got)
	}
	if got := planWindowedCopySegments([]float64{100}, -5); got != nil {
		t.Errorf("negative duration → %v, want nil", got)
	}
}

// A window whose keyframes all sit past the duration (a stale index against a
// replaced file) must not produce a table with inverted or duplicate boundaries.
func TestPlanWindowedCopySegmentsWindowPastDuration(t *testing.T) {
	starts := planWindowedCopySegments([]float64{9000, 9006, 9012}, 600)
	if starts == nil {
		t.Skip("nil is an acceptable rejection for this input")
	}
	checkTableInvariants(t, starts, 600)
}

// The resume position must fall inside a segment whose start is at or before it,
// so the player can begin there without waiting — the actual user-visible
// requirement behind the whole two-phase index.
func TestPlanWindowedCopySegmentsCoversResumePoint(t *testing.T) {
	const resume = 3573.0
	// A window indexed from resume-30 s, as indexKeyframesFast requests.
	window := []float64{3543.2, 3549.9, 3556.1, 3562.7, 3570.0, 3577.4, 3583.9}
	starts := planWindowedCopySegments(window, 8242)

	idx := -1
	for i := 0; i+1 < len(starts); i++ {
		if starts[i] <= resume && resume < starts[i+1] {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("resume %.0f falls in no segment; table=%v", resume, starts)
	}
	// And that segment must start on a real keyframe, else the copy cut is mid-GOP.
	inWindow := false
	for _, kf := range window {
		if math.Abs(kf-starts[idx]) < 0.001 {
			inWindow = true
			break
		}
	}
	if !inWindow {
		t.Errorf("segment %d containing the resume point starts at %.3f, not a keyframe", idx, starts[idx])
	}
}
