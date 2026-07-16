package engine

import (
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/vpn"
	"github.com/anacrolix/torrent"
)

// applyVPNTunnel must close EVERY clear-net leak the default anacrolix client would
// otherwise open, not just wire the tracker dials. This locks in the DHT +
// peer-dialer + inbound-accept hardening: without it, a VPN-required client still
// announces (info_hash, real IP) to the DHT and races a clear-net peer dialer
// against the tunnel — deanonymising the user even with a healthy tunnel.
func TestApplyVPNTunnelHardening(t *testing.T) {
	tcfg := torrent.NewDefaultClientConfig()

	// Precondition: the anacrolix defaults are the leaky ones we must override.
	if !tcfg.DialForPeerConns || !tcfg.AcceptPeerConnections {
		t.Fatal("precondition failed: expected anacrolix defaults DialForPeerConns=true, AcceptPeerConnections=true")
	}
	if tcfg.NoDHT || tcfg.DisableUTP || tcfg.NoDefaultPortForwarding {
		t.Fatal("precondition failed: expected DHT/uTP/port-forwarding ENABLED by default")
	}

	// A device-less tunnel is fine here — applyVPNTunnel only reads the tunnel's
	// method values (it never dials), so no live WireGuard device is needed.
	applyVPNTunnel(tcfg, &vpn.Tunnel{})

	if !tcfg.NoDHT {
		t.Error("NoDHT must be set — DHT on the real UDP socket announces info_hash + real IP to the global DHT")
	}
	if !tcfg.DisableUTP {
		t.Error("DisableUTP must be set — uTP (UDP peers) can't be carried by the tunnel dialer")
	}
	if tcfg.DialForPeerConns {
		t.Error("DialForPeerConns must be false — else the REAL socket dialer races the tunnel dialer for every peer")
	}
	if tcfg.AcceptPeerConnections {
		t.Error("AcceptPeerConnections must be false — else inbound peers reach the real IP:port")
	}
	if !tcfg.NoDefaultPortForwarding {
		t.Error("NoDefaultPortForwarding must be set — no router port map for a leech-only, accept-off client")
	}
	if tcfg.TrackerDialContext == nil || tcfg.HTTPDialContext == nil || tcfg.TrackerListenPacket == nil {
		t.Error("tracker/HTTP dials + UDP tracker announce must be routed through the tunnel")
	}
}
