package cmd

import (
	"log"
	"runtime"
)

// logInboundPeerReachability records, once per daemon start, whether inbound
// peer connections can actually reach this agent on Windows.
//
// Why it earns a line in the boot log: a magnet carries no file list, so the
// client must fetch metadata from the swarm, and a firewalled agent has far
// fewer sources to fetch it from. The failure surfaces as
// "metadata timeout: no peers found" — a message about the torrent, not the
// network — so the user concludes the release is dead and moves on.
//
// Measured on prod 2026-09-03, holding content constant at 10+ seeders:
//
//	windows   16 ok /  60 no-peers = 58.3%
//	linux    104 ok /  16 no-peers = 10.7%
//	darwin    14 ok /   1 no-peers =  4.5%
//
// This deliberately does NOT probe the DHT. An earlier version did, and it was
// wrong twice over:
//
//   - The probe was outbound-initiated. Windows Firewall statefully admits the
//     solicited reply, so it logged "reachable" on precisely the boxes whose
//     inbound port was blocked — it could never observe the condition it was
//     written to detect.
//   - It opened a clear-net UDP socket and pinged the global DHT. For a user
//     with the VPN tunnel active that re-introduces the real-IP leak
//     engine/vpn_wire.go closes on purpose (NoDHT = true, DialForPeerConns =
//     false, AcceptPeerConnections = false), including in the fail-closed state
//     where P2P is supposed to be off entirely.
//
// Reading the firewall rule table instead costs no network traffic, is correct
// with the tunnel up, and answers the question that actually matters.
func logInboundPeerReachability() {
	if runtime.GOOS != "windows" {
		return
	}
	ok, err := windowsFirewallRuleExists()
	if err != nil {
		log.Printf("[firewall] could not read the firewall rules (%v) - "+
			"run `unarr doctor` if downloads fail with \"no peers found\"", err)
		return
	}
	if !ok {
		log.Printf("[firewall] no inbound rule for the agent - fewer peers can reach you, " +
			"and downloads may fail with \"no peers found\" even on well-seeded torrents. " +
			"Fix: re-run `unarr daemon install` from an Administrator prompt")
		return
	}
	log.Printf("[firewall] inbound peer rule present")
}
