package engine

import (
	"context"
	"testing"
	"time"
)

func TestPlaybackHoldPinOutlivesTheLease(t *testing.T) {
	h, clock := newTestHold(30 * time.Second)
	release := h.Pin()
	if !h.Held() {
		t.Fatal("a pin must hold with no lease at all")
	}
	h.Set(false)
	clock.Add(time.Hour)
	if !h.Held() {
		t.Fatal("a server release or a lapsed lease must not free a pinned hold")
	}

	second := h.Pin()
	release()
	release()
	if !h.Held() {
		t.Fatal("a double release of one pin freed another pin's hold")
	}
	second()
	if h.Held() {
		t.Fatal("hold still in force with no pin and no lease")
	}

	h.Set(true)
	p := h.Pin()
	p()
	if !h.Held() {
		t.Fatal("releasing a pin must fall back to the live lease, not clear it")
	}
}

func TestPlaybackHoldWaitsSeePins(t *testing.T) {
	h, _ := newTestHold(30 * time.Second)
	held := make(chan error, 1)
	go func() { held <- h.WaitHeld(context.Background()) }()
	release := h.Pin()
	select {
	case err := <-held:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitHeld did not wake on Pin")
	}

	released := make(chan error, 1)
	go func() { released <- h.WaitReleased(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	release()
	select {
	case err := <-released:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitReleased did not wake on the last unpin")
	}
}
