package engine

import (
	"errors"
	"testing"
	"time"
)

// The P2P stream client is under the SAME kill-switch as the download client: with
// the VPN required and no healthy tunnel, NewStreamEngine must refuse (ErrVPNRequired)
// so streaming a torrent never joins the swarm over the user's real IP. The gate
// returns before any torrent client is constructed (no socket bound, no DHT).
func TestNewStreamEngineVPNGate(t *testing.T) {
	// Required + nil tunnel (never healthy) → refused before any client is created.
	if _, err := NewStreamEngine(StreamConfig{DataDir: t.TempDir(), VPNRequired: true}); !errors.Is(err, ErrVPNRequired) {
		t.Errorf("required + no tunnel: want ErrVPNRequired, got %v", err)
	}
}

// VPNStillHealthy is the throttled mid-stream probe the cmd-layer progress loop
// polls: off → always healthy; on + nil tunnel past the interval → unhealthy
// (fail-closed); a second call within the interval is throttled-healthy (no probe).
func TestStreamEngineVPNStillHealthy(t *testing.T) {
	off := &StreamEngine{cfg: StreamConfig{VPNRequired: false}}
	if !off.VPNStillHealthy() {
		t.Error("kill-switch off must always be healthy")
	}

	// lastVPNCheck is the zero time, so the first call is well past the interval →
	// it probes; the nil tunnel is unhealthy (fail-closed).
	on := &StreamEngine{cfg: StreamConfig{VPNRequired: true}}
	if on.VPNStillHealthy() {
		t.Error("on + nil tunnel (first probe) must be unhealthy — fail-closed")
	}
	// Immediately after, we are within vpnHealthCheckInterval → throttled healthy.
	if !on.VPNStillHealthy() {
		t.Error("within the throttle interval must return healthy without re-probing")
	}
	// Force the throttle window open and confirm it probes (unhealthy) again.
	on.mu.Lock()
	on.lastVPNCheck = time.Now().Add(-2 * vpnHealthCheckInterval)
	on.mu.Unlock()
	if on.VPNStillHealthy() {
		t.Error("past the interval it must re-probe → unhealthy for a nil tunnel")
	}
}
