package mediainfo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The flat 45 s index budget was sized against a warm cache but applied to cold
// reads: measured over NFS, a 12 GB h264 needs 153 s and a 15 GB one 252 s, so
// every cold index died at exactly 45.0 s with "signal: killed ()" and dropped
// the session to EVENT copy — which ignores the resume position entirely.

func TestKeyframeIndexTimeoutScalesWithSize(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)
	cases := []struct {
		name string
		size int64
		want time.Duration
	}{
		// Small files are dominated by mount latency and keyframe density, not by
		// size → the floor holds. A 6 GB telesync needed >180 s cold, which is why
		// the floor is minutes rather than seconds.
		{"tiny clip", 5 * 1024 * 1024, copyKeyframeIndexMinTimeout},
		{"1 GB", gib, copyKeyframeIndexMinTimeout},
		{"6 GB (dense GOP, >180s cold)", 6 * gib, copyKeyframeIndexMinTimeout},
		// The files whose cold indexes were actually measured. The budget must
		// exceed the measurement, else the fix doesn't fix anything.
		{"12 GB (measured 153s cold)", 12 * gib, 480 * time.Second},
		{"15 GB (measured 252s cold)", 15 * gib, 600 * time.Second},
		// A huge remux is bounded by the ceiling so a dead mount can't hang forever.
		{"34 GB", 34 * gib, copyKeyframeIndexMaxTimeout},
		{"200 GB clamps to ceiling", 200 * gib, copyKeyframeIndexMaxTimeout},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "m.mkv")
			f, err := os.Create(p)
			if err != nil {
				t.Fatal(err)
			}
			// Sparse: sets the size without writing the bytes.
			if err := f.Truncate(tc.size); err != nil {
				f.Close()
				t.Skipf("sparse file unsupported: %v", err)
			}
			f.Close()

			if got := keyframeIndexTimeout(p); got != tc.want {
				t.Errorf("size %d → %v, want %v", tc.size, got, tc.want)
			}
		})
	}
}

// Regression guard on the specific numbers: 45 s was the broken value and the
// measured cold times must sit strictly inside the budget.
func TestKeyframeIndexBudgetCoversMeasuredColdTimes(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)
	measured := []struct {
		name string
		size int64
		cold time.Duration
	}{
		{"Mickey 17, 12 GB h264", 12 * gib, 153 * time.Second},
		{"Ballerina, 15 GB h264", 15 * gib, 252 * time.Second},
		// Dense-GOP telesync: blew past a 3-minute budget at only 6 GB, proving
		// size alone does not predict index cost.
		{"Marty Supreme, 6 GB dense h264", 6 * gib, 180 * time.Second},
	}
	for _, m := range measured {
		p := filepath.Join(t.TempDir(), "m.mkv")
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(m.size); err != nil {
			f.Close()
			t.Skipf("sparse file unsupported: %v", err)
		}
		f.Close()

		budget := keyframeIndexTimeout(p)
		if budget <= m.cold {
			t.Errorf("%s: budget %v does not cover measured cold index %v", m.name, budget, m.cold)
		}
		if budget <= 45*time.Second {
			t.Errorf("%s: budget %v regressed to the broken flat 45s", m.name, budget)
		}
	}
}

// An unstattable path must not panic or yield a zero budget (zero would make
// every index fail instantly).
func TestKeyframeIndexTimeoutMissingFile(t *testing.T) {
	got := keyframeIndexTimeout(filepath.Join(t.TempDir(), "nope.mkv"))
	if got != copyKeyframeIndexMinTimeout {
		t.Errorf("missing file → %v, want floor %v", got, copyKeyframeIndexMinTimeout)
	}
}

// A deadline kill must be reported as ErrKeyframeIndexTimeout, not as the bare
// "signal: killed ()" that reads like file corruption.
func TestIndexKeyframesTimeoutIsDistinguishable(t *testing.T) {
	ffprobe, err := ResolveFFprobe("")
	if err != nil {
		t.Skipf("ffprobe unavailable: %v", err)
	}
	media := filepath.Join(t.TempDir(), "m.mkv")
	if err := os.WriteFile(media, []byte("not really a matroska file"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Already-expired deadline → the child is killed before it can finish.
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)

	_, err = IndexKeyframes(ctx, ffprobe, media)
	if err == nil {
		t.Fatal("expected an error from an expired context")
	}
	// The child may lose the race and exit cleanly with a demux error instead of
	// being killed; only assert the classification when it was actually killed.
	if strings.Contains(err.Error(), "signal: killed") {
		t.Errorf("timeout surfaced as a raw kill, not classified: %v", err)
	}
	if errors.Is(err, ErrKeyframeIndexTimeout) && !strings.Contains(err.Error(), "m.mkv") {
		t.Errorf("timeout error should name the file, got: %v", err)
	}
}

// A real demux failure (valid deadline, garbage file) must NOT be misreported as
// a timeout — the two mean opposite things to the caller.
func TestIndexKeyframesRealFailureIsNotTimeout(t *testing.T) {
	ffprobe, err := ResolveFFprobe("")
	if err != nil {
		t.Skipf("ffprobe unavailable: %v", err)
	}
	media := filepath.Join(t.TempDir(), "garbage.mkv")
	if err := os.WriteFile(media, []byte("definitely not media"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = IndexKeyframes(context.Background(), ffprobe, media)
	if err == nil {
		t.Fatal("expected an error indexing a garbage file")
	}
	if errors.Is(err, ErrKeyframeIndexTimeout) {
		t.Errorf("demux failure misclassified as a timeout: %v", err)
	}
}
