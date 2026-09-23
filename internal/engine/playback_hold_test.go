package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestHold(ttl time.Duration) (*PlaybackHold, *fakeClock) {
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	h := NewPlaybackHold(ttl)
	h.now = clock.Now
	return h, clock
}

func TestPlaybackHoldIsALeaseRenewedByReports(t *testing.T) {
	h, clock := newTestHold(30 * time.Second)
	if h.Held() {
		t.Fatal("a new hold must start released")
	}
	h.Set(true)
	clock.Add(20 * time.Second)
	if !h.Held() {
		t.Fatal("hold must still be in force inside its ttl")
	}
	h.Set(true) // renewed by the next sync
	clock.Add(20 * time.Second)
	if !h.Held() {
		t.Fatal("a renewed hold must last another ttl")
	}
	clock.Add(11 * time.Second)
	if h.Held() {
		t.Fatal("a hold nobody renews must lapse on its own")
	}
	h.Set(true)
	h.Set(false)
	if h.Held() {
		t.Fatal("an explicit release must end the hold at once")
	}
}

func TestPlaybackHoldWaitReleasedWakesOnRelease(t *testing.T) {
	h := NewPlaybackHold(time.Minute)
	h.Set(true)
	done := make(chan error, 1)
	go func() { done <- h.WaitReleased(context.Background()) }()
	select {
	case <-done:
		t.Fatal("WaitReleased returned while held")
	case <-time.After(50 * time.Millisecond):
	}
	h.Set(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitReleased: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitReleased did not wake on release")
	}
}

func TestPlaybackHoldWaitReleasedWakesWhenTheLeaseLapses(t *testing.T) {
	h := NewPlaybackHold(80 * time.Millisecond)
	h.Set(true)
	start := time.Now()
	if err := h.WaitReleased(context.Background()); err != nil {
		t.Fatalf("WaitReleased: %v", err)
	}
	if time.Since(start) < 60*time.Millisecond {
		t.Fatal("WaitReleased returned before the lease lapsed")
	}
}

func TestPlaybackHoldWaitsHonourTheContext(t *testing.T) {
	h := NewPlaybackHold(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.WaitHeld(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitHeld on a cancelled ctx = %v, want context.Canceled", err)
	}
	h.Set(true)
	if err := h.WaitReleased(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitReleased on a cancelled ctx = %v, want context.Canceled", err)
	}
}

func TestPlaybackHoldWaitHeldWakesOnHold(t *testing.T) {
	h := NewPlaybackHold(time.Minute)
	done := make(chan error, 1)
	go func() { done <- h.WaitHeld(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	h.Set(false) // a "not held" report must not wake it for good
	h.Set(true)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitHeld: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitHeld did not wake when playback started")
	}
}
