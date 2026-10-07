package cmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/engine"
)

// The stream's source gate holds IPTV downloads from before its first provider
// read until a grace after its release (ffmpeg's socket must be gone first).
func TestHoldIptvForStreamReleasesAfterTheGrace(t *testing.T) {
	prev := iptvStreamReleaseGrace
	iptvStreamReleaseGrace = 50 * time.Millisecond
	t.Cleanup(func() { iptvStreamReleaseGrace = prev })

	hold := engine.NewPlaybackHold(time.Minute)
	dl := engine.NewIptvDownloader(hold)
	release, err := holdIptvForStream(context.Background(), dl, nil, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !hold.Held() {
		t.Fatal("IPTV downloads must be held while the stream reads the provider")
	}
	release()
	if !hold.Held() {
		t.Fatal("released before the grace: a resumed download could race ffmpeg's socket")
	}
	deadline := time.Now().Add(2 * time.Second)
	for hold.Held() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hold.Held() {
		t.Fatal("hold never released after the stream ended")
	}
}

// A stream overtaken while waiting for the slot gives its download pin back at
// once (it never read the provider) and reports the supersession.
func TestHoldIptvForStreamSupersededUnpins(t *testing.T) {
	hold := engine.NewPlaybackHold(time.Minute)
	dl := engine.NewIptvDownloader(hold)
	slot := newProviderStreamSlot(func(string) {})
	releaseA := mustAcquire(t, slot, "a", time.Second)
	defer releaseA()

	errc := make(chan error, 1)
	go func() {
		_, err := holdIptvForStream(context.Background(), dl, slot, "b")
		errc <- err
	}()
	time.Sleep(60 * time.Millisecond)
	// c overtakes b; a never lets go, so c itself sits out its bound.
	go func() { _, _ = slot.acquire(context.Background(), "c", 300*time.Millisecond) }()
	select {
	case err := <-errc:
		if !errors.Is(err, errProviderSuperseded) {
			t.Fatalf("err=%v, want errProviderSuperseded", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("superseded stream never returned")
	}
}
