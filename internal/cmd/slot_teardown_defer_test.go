package cmd

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSlot is a /stream slot whose "someone is reading" state the test drives.
type fakeSlot struct {
	inUse   atomic.Bool
	mu      sync.Mutex
	cleared []uint64
}

func (f *fakeSlot) SlotInUse(uint64, time.Duration) bool { return f.inUse.Load() }
func (f *fakeSlot) ClearFileIf(gen uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, gen)
	return true
}
func (f *fakeSlot) clears() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cleared)
}

// I4: the web closes the session when the viewer opens VLC, which reads the SAME
// /stream. The teardown (slot clear + the session's own release, e.g. the remux
// ffmpeg VLC is reading) must wait until that reader is gone.
func TestSlotTeardownWaitsForTheExternalReader(t *testing.T) {
	prevPoll := slotTeardownPoll
	slotTeardownPoll = 5 * time.Millisecond
	t.Cleanup(func() { slotTeardownPoll = prevPoll })

	slot := &fakeSlot{}
	slot.inUse.Store(true) // VLC attached
	var released atomic.Int32
	cancel := slotSessionCancel(slot, 7, func() { released.Add(1) })
	cancel()
	cancel()
	time.Sleep(50 * time.Millisecond)
	if slot.clears() != 0 || released.Load() != 0 {
		t.Fatalf("teardown cut the external player: clears=%d released=%d", slot.clears(), released.Load())
	}

	slot.inUse.Store(false) // VLC closed
	deadline := time.Now().Add(2 * time.Second)
	for slot.clears() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if slot.clears() != 1 || released.Load() != 1 {
		t.Fatalf("deferred teardown must run exactly once after the reader left: clears=%d released=%d",
			slot.clears(), released.Load())
	}
}

func TestSlotTeardownImmediateWhenNobodyReads(t *testing.T) {
	slot := &fakeSlot{}
	var released atomic.Int32
	slotSessionCancel(slot, 3, func() { released.Add(1) })()
	if slot.clears() != 1 || released.Load() != 1 {
		t.Fatalf("idle slot must be torn down synchronously: clears=%d released=%d", slot.clears(), released.Load())
	}
}
