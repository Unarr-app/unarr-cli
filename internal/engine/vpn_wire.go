package engine

import (
	"github.com/Unarr-app/unarr-cli/internal/vpn"
	"github.com/anacrolix/torrent"
)

// applyVPNTunnel hardens a torrent.ClientConfig so that EVERY outbound path — peer
// dials, tracker announces (HTTP + UDP), web seeds and DHT — routes through the
// WireGuard tunnel, and it closes every clear-net leak the default anacrolix client
// would otherwise open. Shared by the download client (NewTorrentDownloader) and
// the P2P streaming client (NewStreamEngine) so both get identical, auditable
// kill-switch wiring — the two must never drift.
//
// Call it BEFORE torrent.NewClient. Pair it with addVPNDialer AFTER the client is
// created (peer dialers are registered post-construction).
//
// The tunnel's methods fail closed when the inner device is down, so a dead tunnel
// can never fall back to the clear net — it just stops finding peers.
func applyVPNTunnel(tcfg *torrent.ClientConfig, t *vpn.Tunnel) {
	// uTP (UDP peers) can't be carried by the tunnel's TCP dialer, so disable it —
	// TCP peers plus the tunnelled HTTP/UDP tracker announces still find peers.
	tcfg.DisableUTP = true

	// Tracker + web-seed dials and UDP tracker announces go through the tunnel (no
	// IP leak to trackers). Wired through the STABLE Tunnel methods (not a captured
	// *netstack.Net) so a mid-transfer Reconnect that hot-swaps the inner device
	// keeps routing this long-lived client.
	tcfg.TrackerDialContext = t.DialContext
	tcfg.HTTPDialContext = t.DialContext
	tcfg.TrackerListenPacket = t.ListenPacket

	// Close the clear-net leaks the default client would open even with a healthy
	// tunnel:
	//   • DHT binds its server to the REAL UDP socket and, with
	//     PeriodicallyAnnounceTorrentsToDht, publishes (info_hash, real IP) to the
	//     global DHT and answers get_peers from the real IP — a trivial
	//     deanonymisation. TrackerListenPacket does NOT cover DHT, so disable DHT
	//     entirely; the tunnelled trackers still supply peers.
	tcfg.NoDHT = true
	//   • DialForPeerConns=true adds the REAL listen sockets to the client's dialer
	//     pool, so anacrolix dials every peer over BOTH the tunnel dialer AND the
	//     clear-net socket dialer and keeps whichever connects first — the clear SYN
	//     already carries the real IP whoever wins the race. Off → only the
	//     AddDialer'd tunnel dialer (see addVPNDialer) is used for peers.
	tcfg.DialForPeerConns = false
	//   • Don't accept inbound peer connections on the real IP:port, and don't map a
	//     port on the real router — both expose the real IP to peers (leech-only, so
	//     inbound isn't needed anyway).
	tcfg.AcceptPeerConnections = false
	tcfg.NoDefaultPortForwarding = true
}

// addVPNDialer routes outgoing TCP peer dials through the tunnel. Call it AFTER
// torrent.NewClient (peer dialers are registered post-construction). Because
// applyVPNTunnel set DialForPeerConns=false, this tunnel dialer is the ONLY peer
// dialer, so no clear-net peer dial can race it. The Tunnel itself is the
// context-aware dialer, so a Reconnect transparently re-routes peer dials too.
func addVPNDialer(client *torrent.Client, t *vpn.Tunnel) {
	client.AddDialer(torrent.NetworkDialer{Network: "tcp", Dialer: t})
}
