package library

import (
	"context"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

// A huge ratio means the threshold is always above the real load, so the gate
// must return immediately (no blocking) regardless of how busy the box is.
func TestWaitForLowLoad_HighRatioReturnsImmediately(t *testing.T) {
	done := make(chan struct{})
	go func() {
		waitForLowLoad(context.Background(), 1e9)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitForLowLoad blocked despite an impossibly-high threshold")
	}
}

// With a tiny ratio the gate would block (load almost always exceeds it), but a
// cancelled context must unblock it promptly — the prewarm has to stop cleanly on
// Ctrl-C / daemon shutdown even while waiting for the machine to go idle.
func TestWaitForLowLoad_RespectsContextCancel(t *testing.T) {
	if _, ok := mediainfo.LoadAverage1(); !ok {
		t.Skip("no load reading on this platform — gate is a no-op")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	done := make(chan struct{})
	go func() {
		waitForLowLoad(ctx, 0.0001) // threshold ~0 → would otherwise block
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitForLowLoad ignored a cancelled context")
	}
}
