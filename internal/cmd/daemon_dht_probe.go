package cmd

import (
	"log"
	"runtime"
)

// logDHTReachability records, once per daemon start, whether this machine can
// complete a DHT round trip.
//
// Why it earns a line in the boot log: a magnet link carries no file list, so
// the client must fetch its metadata from the swarm, and the DHT is how it finds
// the swarm. Block inbound UDP and every magnet download ends as
// "metadata timeout: no peers found" — a message that describes the torrent,
// not the network, so the user concludes the release is dead and moves on.
//
// Measured on prod 2026-09-03, holding the content constant at 10+ seeders:
//
//	windows   16 ok /  60 no-peers = 58.3%
//	linux    104 ok /  16 no-peers = 10.7%
//	darwin    14 ok /   1 no-peers =  4.5%
//
// Same 1.11.5 build everywhere. `unarr doctor` could already diagnose this on
// demand, but it is a command a user runs only once they suspect the network —
// and this failure never points there. Logging it at startup is what turns a
// silent misconfiguration into something support can read off a log.
func logDHTReachability() {
	nodes, err := probeDHT()
	if err != nil {
		log.Printf("[dht] bootstrap unreachable (%v) - magnet downloads may fail with "+
			"\"no peers found\" even on well-seeded torrents%s", err, dhtHintForOS())
		return
	}
	if nodes == 0 {
		log.Printf("[dht] no bootstrap node answered - magnet downloads may fail with "+
			"\"no peers found\" even on well-seeded torrents%s", dhtHintForOS())
		return
	}
	log.Printf("[dht] reachable (%d bootstrap node(s) answered)", nodes)
}

// dhtHintForOS names the most likely culprit per platform. On Windows that is
// overwhelmingly the built-in firewall, which is why `daemon install` now adds
// the rule itself — this hint covers agents installed before that, and installs
// that ran without elevation.
func dhtHintForOS() string {
	if runtime.GOOS == "windows" {
		return "; on Windows this is usually Windows Defender Firewall - run `unarr daemon install` " +
			"from an Administrator prompt to add the inbound rule, then `unarr doctor` to confirm"
	}
	return "; check that outbound UDP is allowed and that any VPN is not blocking it - `unarr doctor` has the detail"
}
