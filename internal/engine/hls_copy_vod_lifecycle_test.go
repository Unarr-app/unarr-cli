package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
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

func TestCopyVODWaitCancellation(t *testing.T) {
	for _, gateBlocked := range []bool{true, false} {
		s := lazyLifecycleSession(t)
		if gateBlocked {
			s.copyGenGate(0) <- struct{}{}
		} else {
			s.copySlots <- struct{}{}
			s.copySlots <- struct{}{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := s.ensureCopySegment(ctx, 0)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("blocked request: %v", err)
		}
	}
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
	if err := s.ensureCopySegment(context.Background(), -1); err == nil {
		t.Fatal("accepted negative index")
	}
	if err := s.ensureCopySegment(context.Background(), 2); err == nil {
		t.Fatal("accepted out-of-range index")
	}
}
