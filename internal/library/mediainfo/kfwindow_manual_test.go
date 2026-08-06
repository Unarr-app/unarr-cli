//go:build kfbench

package mediainfo

// Manual A/B of the windowed vs full keyframe index over a real mount.
//
//	go test ./internal/library/mediainfo/ -tags kfbench -run TestKeyframeWindowBench \
//	  -timeout 30m -v -args -kfbench.media="/path/file.mkv" -kfbench.from=3573

import (
	"context"
	"flag"
	"testing"
	"time"
)

var (
	benchFrom   = flag.Float64("kfbench.from", 3573, "resume position in seconds")
	benchWindow = flag.Float64("kfbench.window", 300, "window length in seconds")
)

func TestKeyframeWindowBench(t *testing.T) {
	if *benchMedia == "" {
		t.Skip("set -args -kfbench.media=<path>")
	}
	ffprobe, err := ResolveFFprobe("")
	if err != nil {
		t.Skipf("ffprobe unavailable: %v", err)
	}

	t0 := time.Now()
	win, err := IndexKeyframeWindow(context.Background(), ffprobe, *benchMedia, *benchFrom, *benchWindow)
	winElapsed := time.Since(t0)
	if err != nil {
		t.Fatalf("WINDOW failed after %v: %v", winElapsed, err)
	}
	t.Logf("WINDOW from=%.0f len=%.0f → n=%d elapsed=%v (first=%.3f last=%.3f)",
		*benchFrom, *benchWindow, len(win), winElapsed, win[0], win[len(win)-1])

	t1 := time.Now()
	full, err := IndexKeyframes(context.Background(), ffprobe, *benchMedia)
	fullElapsed := time.Since(t1)
	if err != nil {
		t.Fatalf("FULL failed after %v: %v", fullElapsed, err)
	}
	t.Logf("FULL n=%d elapsed=%v", len(full), fullElapsed)
	t.Logf("SPEEDUP %.1fx", float64(fullElapsed)/float64(winElapsed))

	// The window must be an exact subset of the full index, not an approximation.
	inFull := make(map[float64]bool, len(full))
	for _, v := range full {
		inFull[v] = true
	}
	for _, v := range win {
		if !inFull[v] {
			t.Errorf("window keyframe %.6f absent from the full index", v)
		}
	}
	// And it must actually bracket the requested resume point.
	if win[0] > *benchFrom {
		t.Errorf("window starts at %.3f, past the requested %.0f — no seekable boundary", win[0], *benchFrom)
	}
}
