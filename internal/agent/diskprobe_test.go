package agent

import (
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// setDiskProbeSeam publishes the seam under the probe mutex — the same lock
// DiskInfoBounded snapshots it with. A parked probe goroutine outlives the test
// that started it, so an unsynchronised swap here is a genuine data race, not a
// test-only detail (the race detector caught exactly that).
func setDiskProbeSeam(fn func(string) (int64, int64, error), timeout time.Duration) {
	diskProbeMu.Lock()
	defer diskProbeMu.Unlock()
	diskInfoFn, diskProbeTimeout = fn, timeout
}

// probePath gives each test a private path. A stub probe parked in a syscall
// outlives the test that started it — when it finally returns it clears the
// single-flight slot for its path. With a shared path that late write could
// unlock a LATER test's in-flight probe and let a second one start, which is
// exactly how TestDiskInfoBoundedSingleFlight flaked (2 probes, want 1).
func probePath(t *testing.T) string {
	t.Helper()
	return "/probe/" + t.Name()
}

// withStubDiskInfo swaps the syscall seam and the timeout for the duration of a
// test, restoring both (and the per-path caches) afterwards.
func withStubDiskInfo(t *testing.T, timeout time.Duration, fn func(string) (int64, int64, error)) {
	t.Helper()
	diskProbeMu.Lock()
	origFn, origTimeout := diskInfoFn, diskProbeTimeout
	diskProbeMu.Unlock()

	setDiskProbeSeam(fn, timeout)
	resetDiskProbeState()
	t.Cleanup(func() {
		setDiskProbeSeam(origFn, origTimeout)
		resetDiskProbeState()
	})
}

// TestDiskInfoBoundedHealthyPassesThrough is the no-regression guard: on a
// filesystem that answers, the bounded probe must return exactly what the bare
// syscall returns. Everything else here is about failure modes; this is the one
// that guarantees the normal path is untouched.
func TestDiskInfoBoundedHealthyPassesThrough(t *testing.T) {
	withStubDiskInfo(t, time.Second, func(string) (int64, int64, error) {
		return 111, 222, nil
	})
	free, total, err := DiskInfoBounded(probePath(t))
	if err != nil {
		t.Fatalf("healthy probe returned an error: %v", err)
	}
	if free != 111 || total != 222 {
		t.Errorf("got free=%d total=%d, want 111/222", free, total)
	}
}

// TestDiskInfoBoundedRealPathAgrees checks the seam did not change the meaning
// of the call: the bounded probe over a real directory must agree with DiskInfo.
func TestDiskInfoBoundedRealPathAgrees(t *testing.T) {
	resetDiskProbeState()
	t.Cleanup(resetDiskProbeState)
	dir := t.TempDir()
	wantFree, wantTotal, err := DiskInfo(dir)
	if err != nil {
		t.Skipf("DiskInfo unavailable on this platform/path: %v", err)
	}
	gotFree, gotTotal, err := DiskInfoBounded(dir)
	if err != nil {
		t.Fatalf("bounded probe failed on a real dir: %v", err)
	}
	if gotTotal != wantTotal {
		t.Errorf("total mismatch: bounded=%d bare=%d", gotTotal, wantTotal)
	}
	// Free space can legitimately move between the two calls on a live box, so
	// assert it is in the same ballpark rather than exactly equal.
	if gotFree <= 0 || wantFree <= 0 {
		return
	}
	if ratio := float64(gotFree) / float64(wantFree); ratio < 0.5 || ratio > 2 {
		t.Errorf("free space wildly different: bounded=%d bare=%d", gotFree, wantFree)
	}
}

// TestDiskInfoBoundedTimesOut is the core fix: a probe that never returns must
// not take the caller with it. The bare DiskInfo here would block the test
// forever, which is exactly what it did to the daemon at startup.
func TestDiskInfoBoundedTimesOut(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	withStubDiskInfo(t, 30*time.Millisecond, func(string) (int64, int64, error) {
		<-release // parked, like statfs on a dead SMB share
		return 0, 0, nil
	})

	start := time.Now()
	if _, _, err := DiskInfoBounded(probePath(t)); err == nil {
		t.Fatal("a probe that never answers must report an error, not succeed")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("caller waited %s — the timeout did not bound it", elapsed)
	}
}

// TestDiskInfoBoundedSingleFlight guards the thread leak. A goroutine blocked in
// an uninterruptible syscall cannot be cancelled, so the heartbeat calling this
// every few seconds must NOT keep spawning new ones against the same dead path.
func TestDiskInfoBoundedSingleFlight(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	// Buffered + non-blocking send: the stub must never block on this, only
	// announce that the probe goroutine actually got scheduled.
	started := make(chan struct{}, 1)
	t.Cleanup(func() { close(release) })
	withStubDiskInfo(t, 20*time.Millisecond, func(string) (int64, int64, error) {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return 0, 0, nil
	})

	// Wait for the FIRST probe to be running before counting. The probe runs on
	// its own goroutine, so reading the counter straight after the call can
	// observe 0 — which is not the failure this test is about (that would be a
	// probe that never started, not one that started twice). Waiting here is what
	// makes the assertion below mean "exactly one", every time.
	_, _, _ = DiskInfoBounded(probePath(t))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first probe goroutine never ran")
	}

	// Now the interesting part: further calls against the same stuck path must be
	// served without spawning anything, or the heartbeat would leak a thread every
	// few seconds.
	for range 4 {
		_, _, _ = DiskInfoBounded(probePath(t))
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("started %d probes against one stuck path, want exactly 1", n)
	}
}

// TestDiskInfoBoundedServesLastGoodSample: once a path has answered, a later
// stall should degrade to slightly stale numbers rather than to no numbers.
func TestDiskInfoBoundedServesLastGoodSample(t *testing.T) {
	var stall atomic.Bool
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	withStubDiskInfo(t, 30*time.Millisecond, func(string) (int64, int64, error) {
		if stall.Load() {
			<-release
		}
		return 500, 1000, nil
	})

	if _, _, err := DiskInfoBounded(probePath(t)); err != nil {
		t.Fatalf("warm-up probe failed: %v", err)
	}
	stall.Store(true)
	free, total, err := DiskInfoBounded(probePath(t)) // this one wedges
	if err != nil {
		t.Fatalf("stalled probe with a cached sample must not error: %v", err)
	}
	if free != 500 || total != 1000 {
		t.Errorf("got free=%d total=%d, want the cached 500/1000", free, total)
	}
	// And the in-flight probe must still be single-flighted afterwards.
	if _, _, err := DiskInfoBounded(probePath(t)); err != nil {
		t.Errorf("follow-up call while stuck must serve the cache, got %v", err)
	}
}

// TestDiskInfoBoundedPropagatesError: a path that genuinely cannot be measured
// (missing dir) must still surface an error, so callers omit the disk fields
// instead of reporting zeros as real numbers.
func TestDiskInfoBoundedPropagatesError(t *testing.T) {
	withStubDiskInfo(t, time.Second, func(string) (int64, int64, error) {
		return 0, 0, errors.New("no such file or directory")
	})
	if _, _, err := DiskInfoBounded(probePath(t)); err == nil {
		t.Error("a failing probe must return an error")
	}
}

// TestDiskInfoBoundedRecovers: a share that comes back must be measured again,
// not pinned to the cached sample forever.
func TestDiskInfoBoundedRecovers(t *testing.T) {
	var blocked atomic.Bool
	blocked.Store(true)
	release := make(chan struct{})
	withStubDiskInfo(t, 20*time.Millisecond, func(string) (int64, int64, error) {
		if blocked.Load() {
			<-release
		}
		return 7, 8, nil
	})

	if _, _, err := DiskInfoBounded(probePath(t)); err == nil {
		t.Fatal("expected the first (stuck) probe to time out")
	}
	blocked.Store(false)
	close(release) // the parked probe finishes and clears the single-flight slot

	// Poll briefly: the unblocked goroutine clears `busy` asynchronously.
	deadline := time.Now().Add(2 * time.Second)
	for {
		free, total, err := DiskInfoBounded(probePath(t))
		if err == nil && free == 7 && total == 8 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe never recovered (last: free=%d total=%d err=%v)", free, total, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDiskInfoBoundedIsUsedByRegister pins the wiring: the bare DiskInfo must
// not creep back into the daemon-lifetime call sites, which is the whole reason
// the bounded variant exists.
func TestDiskInfoBoundedIsUsedByRegister(t *testing.T) {
	for _, f := range []string{"daemon.go", "sync.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !strings.Contains(string(src), "DiskInfoBounded(") {
			t.Errorf("%s no longer uses DiskInfoBounded — a bare DiskInfo there can wedge the agent", f)
		}
	}
}
