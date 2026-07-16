package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestTorrentAvailableVPNGate verifies the authoritative start-of-task gate on the
// torrent downloader directly (no anacrolix client needed — Available only reads
// the task + the VPN gate). With the kill-switch on and no healthy tunnel it
// reports ErrVPNRequired so resolveMethod never selects torrent; off, it's
// available (historical behavior); no info hash is always unavailable.
func TestTorrentAvailableVPNGate(t *testing.T) {
	task := &Task{InfoHash: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}

	// Kill-switch on, nil tunnel (never healthy) → gated, clear reason.
	gated := &TorrentDownloader{cfg: TorrentConfig{VPNRequired: true}}
	if ok, err := gated.Available(context.Background(), task); ok || !errors.Is(err, ErrVPNRequired) {
		t.Errorf("required + no tunnel: got (%v, %v), want (false, ErrVPNRequired)", ok, err)
	}

	// Kill-switch off → available even without a tunnel (best-effort clear-net path).
	off := &TorrentDownloader{cfg: TorrentConfig{VPNRequired: false}}
	if ok, err := off.Available(context.Background(), task); !ok || err != nil {
		t.Errorf("not required: got (%v, %v), want (true, nil)", ok, err)
	}

	// No info hash → unavailable with no error, even with the kill-switch on
	// (torrent simply can't handle the task; nothing to gate).
	if ok, err := gated.Available(context.Background(), &Task{InfoHash: ""}); ok || err != nil {
		t.Errorf("empty info hash: got (%v, %v), want (false, nil)", ok, err)
	}
}

// TestTorrentTunnelHealthy verifies the gate helper: off → always satisfied;
// on + nil tunnel → not satisfied (nil-safe, fail-closed).
func TestTorrentTunnelHealthy(t *testing.T) {
	if !(&TorrentDownloader{cfg: TorrentConfig{VPNRequired: false}}).tunnelHealthy() {
		t.Error("kill-switch off must always be satisfied")
	}
	if (&TorrentDownloader{cfg: TorrentConfig{VPNRequired: true}}).tunnelHealthy() {
		t.Error("kill-switch on with a nil tunnel must be unsatisfied (fail-closed)")
	}
}

// TestTorrentVPNStillHealthy covers the throttled mid-download probe: off → always
// healthy; on + past the interval → probes (nil tunnel = unhealthy) and advances
// *last; on + within the interval → throttled healthy without advancing *last. A
// regression that inverted the throttle or broke the fail-closed drop check would be
// caught here without needing a live WireGuard device.
func TestTorrentVPNStillHealthy(t *testing.T) {
	off := &TorrentDownloader{cfg: TorrentConfig{VPNRequired: false}}
	last := time.Time{}
	if !off.vpnStillHealthy(&last, time.Now()) {
		t.Error("kill-switch off must always be healthy")
	}

	on := &TorrentDownloader{cfg: TorrentConfig{VPNRequired: true}}
	now := time.Now()
	last = time.Time{} // zero → well past the interval → this call probes
	if on.vpnStillHealthy(&last, now) {
		t.Error("on + nil tunnel past the interval must be unhealthy (fail-closed)")
	}
	if !last.Equal(now) {
		t.Error("a real probe must advance *last to now")
	}
	// Within the interval → throttled healthy, *last must NOT advance.
	if !on.vpnStillHealthy(&last, now.Add(time.Second)) {
		t.Error("within the throttle interval must return healthy without probing")
	}
	if !last.Equal(now) {
		t.Error("a throttled call must NOT advance *last")
	}
}
