package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func lazyLifecycleSession(t *testing.T) *HLSSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &HLSSession{copyVOD: true, copyLazy: true, copyCtx: ctx, copyCancel: cancel, copySlots: make(chan struct{}, 2), segmentCount: 2, copySegStarts: []float64{0, 6, 12}, tmpDir: t.TempDir()}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// fakeCopyPipeline is a lazy session whose "ffmpeg" is a controllable stub:
// each generation blocks until released, then publishes a real segment file.
type fakeCopyPipeline struct {
	s       *HLSSession
	mu      sync.Mutex
	started []int
	aborted []int
	gate    map[int]chan struct{}
	running atomic.Int32
	maxPar  atomic.Int32
}

func newFakeCopyPipeline(t *testing.T, segments int, prefetch bool) *fakeCopyPipeline {
	t.Helper()
	s := lazyLifecycleSession(t)
	s.segmentCount = segments
	if err := os.MkdirAll(filepath.Join(s.tmpDir, "video"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeCopyPipeline{s: s, gate: make(map[int]chan struct{})}
	s.copyGenerate = f.generate
	if prefetch {
		s.copyWake = make(chan struct{}, 1)
		s.copyWG.Add(1)
		go s.runCopyPrefetch()
	}
	return f
}

func (f *fakeCopyPipeline) gateFor(idx int) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate[idx] == nil {
		f.gate[idx] = make(chan struct{})
	}
	return f.gate[idx]
}

func (f *fakeCopyPipeline) release(idx int) { close(f.gateFor(idx)) }

func (f *fakeCopyPipeline) generate(ctx context.Context, idx int) error {
	n := f.running.Add(1)
	defer f.running.Add(-1)
	for {
		if prev := f.maxPar.Load(); n <= prev || f.maxPar.CompareAndSwap(prev, n) {
			break
		}
	}
	f.mu.Lock()
	f.started = append(f.started, idx)
	f.mu.Unlock()
	select {
	case <-f.gateFor(idx):
	case <-ctx.Done():
		f.mu.Lock()
		f.aborted = append(f.aborted, idx)
		f.mu.Unlock()
		return ctx.Err()
	}
	return os.WriteFile(f.s.copySegPath(idx), []byte("ts"), 0o600)
}

func (f *fakeCopyPipeline) snapshot() (started, aborted []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.started...), append([]int(nil), f.aborted...)
}

// THE regression: hls.js abandons a fragment request after ~10 s without a first
// byte. That must not kill the generation — the retry has to find the work done
// (or join it), otherwise a slow segment can never be produced at all.
func TestCopyVODAbandonedRequestKeepsGenerating(t *testing.T) {
	f := newFakeCopyPipeline(t, 2, false)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := f.s.ensureCopySegment(ctx, 0)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("abandoned request: %v", err)
	}
	if _, aborted := f.snapshot(); len(aborted) != 0 {
		t.Fatalf("generation was killed by the request abort: %v", aborted)
	}

	retry := make(chan error, 1)
	go func() { retry <- f.s.ensureCopySegment(context.Background(), 0) }()
	time.Sleep(20 * time.Millisecond)
	f.release(0)
	if err := <-retry; err != nil {
		t.Fatalf("retry: %v", err)
	}
	if started, _ := f.snapshot(); len(started) != 1 {
		t.Fatalf("retry restarted the work instead of joining it: runs=%v", started)
	}
}

func TestCopyVODConcurrentRequestsShareOneRun(t *testing.T) {
	f := newFakeCopyPipeline(t, 2, false)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.s.ensureCopySegment(context.Background(), 1); err != nil {
				t.Errorf("waiter: %v", err)
			}
		}()
	}
	waitFor(t, "generation start", func() bool { st, _ := f.snapshot(); return len(st) > 0 })
	time.Sleep(20 * time.Millisecond)
	f.release(1)
	wg.Wait()
	if started, _ := f.snapshot(); len(started) != 1 {
		t.Fatalf("runs = %v, want exactly one", started)
	}
}

// After the viewer's segment, the prefetcher fills the window one segment at a
// time — never in parallel with the demand, never past the lookahead.
func TestCopyVODPrefetchIsSequentialAndBounded(t *testing.T) {
	f := newFakeCopyPipeline(t, 20, true)
	for i := 0; i < 20; i++ {
		f.release(i)
	}
	if err := f.s.ensureCopySegment(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	last := 5 + copyVODLookahead
	waitFor(t, "prefetch window", func() bool { return f.s.copySegmentReady(last) })
	time.Sleep(30 * time.Millisecond) // give an overshoot the chance to show up
	started, _ := f.snapshot()
	want := []int{5, 6, 7, 8}
	if len(started) != len(want) {
		t.Fatalf("generated %v, want %v", started, want)
	}
	for i := range want {
		if started[i] != want[i] {
			t.Fatalf("generated %v, want %v (in order)", started, want)
		}
	}
	if f.maxPar.Load() != 1 {
		t.Fatalf("prefetch ran %d generations in parallel", f.maxPar.Load())
	}

	// Playback advancing one segment tops the window up by exactly one.
	if err := f.s.ensureCopySegment(context.Background(), 6); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "window top-up", func() bool { return f.s.copySegmentReady(last + 1) })
	if started, _ := f.snapshot(); len(started) != 5 || started[4] != last+1 {
		t.Fatalf("after advance generated %v", started)
	}
}

// A seek must stop prefetching the OLD position so it cannot compete with the
// segment the viewer is now waiting for.
func TestCopyVODSeekCancelsStalePrefetch(t *testing.T) {
	f := newFakeCopyPipeline(t, 200, true)
	f.release(10)
	if err := f.s.ensureCopySegment(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "prefetch of 11 to start", func() bool {
		st, _ := f.snapshot()
		return len(st) == 2 && st[1] == 11
	})
	f.release(150)
	if err := f.s.ensureCopySegment(context.Background(), 150); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stale prefetch abort", func() bool {
		_, ab := f.snapshot()
		return len(ab) == 1 && ab[0] == 11
	})
	if f.s.copySegmentReady(11) {
		t.Fatal("cancelled prefetch published a segment")
	}
	waitFor(t, "prefetch at new position", func() bool {
		st, _ := f.snapshot()
		return len(st) > 0 && st[len(st)-1] == 151
	})
}

func TestCopyVODCloseCancelsWaiters(t *testing.T) {
	s := lazyLifecycleSession(t)
	s.copySlots <- struct{}{}
	s.copySlots <- struct{}{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.ensureCopySegment(context.Background(), 0); !errors.Is(err, context.Canceled) {
				t.Errorf("close: %v", err)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waiters survived Close")
	}
	if _, err := os.Stat(s.tmpDir); !os.IsNotExist(err) {
		t.Fatalf("session directory survived close: %v", err)
	}
	if err := s.ensureCopySegment(context.Background(), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("request after Close: %v", err)
	}
}

func TestCopyVODCloseStopsPrefetcher(t *testing.T) {
	f := newFakeCopyPipeline(t, 50, true)
	f.release(0)
	if err := f.s.ensureCopySegment(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "prefetch in flight", func() bool { st, _ := f.snapshot(); return len(st) == 2 })
	closed := make(chan error, 1)
	go func() { closed <- f.s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on the prefetcher")
	}
}

func TestCopyVODFailedGenerationIsNotPublished(t *testing.T) {
	s := lazyLifecycleSession(t)
	s.cfg.Transcode.FFmpegPath = filepath.Join(t.TempDir(), "missing-ffmpeg")
	s.probe = &StreamProbe{}
	if err := os.MkdirAll(filepath.Join(s.tmpDir, "video"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureCopySegment(context.Background(), 0); err == nil {
		t.Fatal("expected encoder error")
	}
	for _, p := range []string{s.copySegPath(0), s.copySegPath(0) + ".tmp"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("published failed segment: %s", p)
		}
	}
	// A failure must not be cached: the next request runs again.
	if err := s.ensureCopySegment(context.Background(), 0); err == nil {
		t.Fatal("expected encoder error on retry")
	}
	if err := s.ensureCopySegment(context.Background(), -1); err == nil {
		t.Fatal("accepted negative index")
	}
	if err := s.ensureCopySegment(context.Background(), 2); err == nil {
		t.Fatal("accepted out-of-range index")
	}
}
