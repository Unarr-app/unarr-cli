package vpn

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestTunnelHealthyNilSafe: a nil tunnel and a device-less (failed-start /
// pre-reconnect) tunnel are both unhealthy — the kill-switch fails closed.
func TestTunnelHealthyNilSafe(t *testing.T) {
	var nilTunnel *Tunnel
	if nilTunnel.Healthy() {
		t.Error("nil tunnel must be unhealthy (fail-closed)")
	}
	if (&Tunnel{}).Healthy() {
		t.Error("device-less tunnel must be unhealthy (fail-closed)")
	}
}

// TestTunnelDialsFailClosedWhenDown: with no live device, both dial paths error
// instead of falling back to the clear net — this is the no-IP-leak guarantee.
func TestTunnelDialsFailClosedWhenDown(t *testing.T) {
	down := &Tunnel{}
	if _, err := down.DialContext(context.Background(), "tcp", "1.2.3.4:80"); err == nil {
		t.Error("DialContext on a down tunnel must error (fail-closed)")
	}
	if _, err := down.ListenPacket("udp", ":0"); err == nil {
		t.Error("ListenPacket on a down tunnel must error (fail-closed)")
	}
}

// TestReconnectNilTunnel: Reconnect on a nil receiver is a clear error, not a panic.
func TestReconnectNilTunnel(t *testing.T) {
	var nilTunnel *Tunnel
	if err := nilTunnel.Reconnect("whatever"); err == nil {
		t.Error("Reconnect on a nil tunnel must return an error")
	}
}

// TestHandshakeFresh covers the pure liveness decision without a live WireGuard
// device: a recent handshake is live, a stale one is dead, and a not-yet-handshaked
// tunnel is only live inside the post-Up grace window.
func TestHandshakeFresh(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	handshake := func(secondsAgo int64) string {
		return fmt.Sprintf("public_key=abc\nendpoint=1.2.3.4:51820\nlast_handshake_time_sec=%d\nlast_handshake_time_nsec=0\n",
			now.Add(-time.Duration(secondsAgo)*time.Second).Unix())
	}

	tests := []struct {
		name      string
		ipc       string
		startedAt time.Time
		want      bool
	}{
		{"fresh handshake 60s ago", handshake(60), now.Add(-10 * time.Minute), true},
		{"handshake exactly at stale threshold", handshake(int64(handshakeStaleAfter.Seconds())), now.Add(-10 * time.Minute), true},
		{"stale handshake 300s ago", handshake(300), now.Add(-10 * time.Minute), false},
		{"no handshake within post-up grace", "public_key=abc\nlast_handshake_time_sec=0\n", now.Add(-5 * time.Second), true},
		{"no handshake past post-up grace", "public_key=abc\nlast_handshake_time_sec=0\n", now.Add(-2 * time.Minute), false},
		{"no handshake line at all, within grace", "public_key=abc\nendpoint=1.2.3.4:51820\n", now.Add(-5 * time.Second), true},
		{"multi-peer: newest fresh handshake wins", "last_handshake_time_sec=" +
			fmt.Sprintf("%d", now.Add(-9*time.Minute).Unix()) + "\nlast_handshake_time_sec=" +
			fmt.Sprintf("%d", now.Add(-30*time.Second).Unix()) + "\n", now.Add(-10 * time.Minute), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := handshakeFresh(tt.ipc, now, tt.startedAt); got != tt.want {
				t.Errorf("handshakeFresh() = %v, want %v", got, tt.want)
			}
		})
	}
}
