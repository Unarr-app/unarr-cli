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

// testConf is a minimal but valid WireGuard .conf (32-zero-byte keys, literal-IP
// endpoint so no DNS is needed) that bringUp can turn into a userspace device in a
// unit test — no network, no root.
func testConf(endpoint string) string {
	const zeroKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	return "[Interface]\nPrivateKey = " + zeroKey + "\nAddress = 10.0.0.2/32\nDNS = 1.1.1.1\n\n" +
		"[Peer]\nPublicKey = " + zeroKey + "\nEndpoint = " + endpoint + "\nAllowedIPs = 0.0.0.0/0\n"
}

// TestReconnectPopulatesEndpointWhenEmpty: a fail-closed tunnel that never started
// (empty Endpoint) gets the resolved exit server recorded on the first successful
// Reconnect, so `unarr vpn status` can show the "Exit server" line after healing.
func TestReconnectPopulatesEndpointWhenEmpty(t *testing.T) {
	tun := &Tunnel{} // device-less, no Endpoint — mirrors failedTunnel(required=true)
	if tun.Endpoint != "" {
		t.Fatalf("precondition: expected empty Endpoint, got %q", tun.Endpoint)
	}
	if err := tun.Reconnect(testConf("1.2.3.4:51820")); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	defer tun.Close()
	if tun.Endpoint != "1.2.3.4:51820" {
		t.Errorf("Endpoint after heal = %q, want %q", tun.Endpoint, "1.2.3.4:51820")
	}
}

// TestReconnectKeepsExistingEndpoint: a healthy tunnel that already exits through a
// known server keeps that Endpoint across a Reconnect (same exit server invariant) —
// a mid-session reconnect must not rewrite or blank the displayed exit server.
func TestReconnectKeepsExistingEndpoint(t *testing.T) {
	tun, err := Up(testConf("1.2.3.4:51820"))
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer tun.Close()
	if tun.Endpoint != "1.2.3.4:51820" {
		t.Fatalf("Up Endpoint = %q, want %q", tun.Endpoint, "1.2.3.4:51820")
	}
	// Reconnect against a config pointing at a different endpoint: the already-set
	// label survives unchanged (Up is the source of truth for the exit server).
	if err := tun.Reconnect(testConf("5.6.7.8:51820")); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if tun.Endpoint != "1.2.3.4:51820" {
		t.Errorf("Endpoint after Reconnect = %q, want it unchanged %q", tun.Endpoint, "1.2.3.4:51820")
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
