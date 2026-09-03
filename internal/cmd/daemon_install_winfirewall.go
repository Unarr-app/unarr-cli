package cmd

import (
	"fmt"
	"strings"

	"github.com/fatih/color"
)

// Windows Defender Firewall blocks inbound connections to a new binary by
// default. For a BitTorrent client that is not a cosmetic loss: without inbound
// UDP the DHT never completes a round trip, and a magnet link has no other way
// to fetch its metadata — the download dies as "no peers found" even when the
// swarm is healthy.
//
// Measured on prod 2026-09-03, restricted to torrents with 10+ seeders so the
// content is held constant:
//
//	windows   16 ok /  60 no-peers = 58.3%   (median 46 seeders)
//	linux    104 ok /  16 no-peers = 10.7%   (median 21 seeders)
//	darwin    14 ok /   1 no-peers =  4.5%
//
// Same agent build (1.11.5) in all three, and the Windows users were asking for
// BETTER-seeded torrents than the Linux ones. The platform was the variable.
//
// The rules are named so `netsh advfirewall firewall delete rule` can find them
// on uninstall, and so a user can see what we added.
const (
	winFirewallRuleTCP = "unarr (peer TCP)"
	winFirewallRuleUDP = "unarr (peer UDP / DHT)"
)

// addWindowsFirewallRules allows inbound peer traffic to listenPort.
//
// It never fails the install: creating a firewall rule needs an elevated
// process, and a non-elevated install is still a working agent for everything
// EXCEPT inbound peers. So a refusal is reported with the exact command to run,
// not swallowed and not fatal — the whole point of this function is that the
// user finds out, since the symptom otherwise shows up much later as downloads
// that "just fail".
func addWindowsFirewallRules(listenPort int, green *color.Color) {
	if listenPort <= 0 {
		// listen_port 0 means "random each start", so there is no single port to
		// authorise. Say so: it is a real reason inbound peers will not work.
		color.New(color.FgYellow).Println(
			"  Note: downloads.listen_port is 0 (random each start), so no firewall rule was added.",
		)
		fmt.Println("        Set a fixed port in the config and re-run `unarr daemon install`")
		fmt.Println("        to let peers reach you — without it, magnet downloads can fail")
		fmt.Println("        with \"no peers found\" even on well-seeded torrents.")
		return
	}

	var failed []string
	for _, r := range []struct {
		name  string
		proto string
	}{
		{winFirewallRuleTCP, "TCP"},
		{winFirewallRuleUDP, "UDP"},
	} {
		// Delete first so a re-install after a port change does not leave a stale
		// rule pointing at the old port. A missing rule makes delete exit
		// non-zero, which is fine and deliberately ignored.
		_, _ = svcOutput("netsh", "advfirewall", "firewall", "delete", "rule", "name="+r.name)

		out, err := svcOutput("netsh", "advfirewall", "firewall", "add", "rule",
			"name="+r.name,
			"dir=in",
			"action=allow",
			"protocol="+r.proto,
			fmt.Sprintf("localport=%d", listenPort),
			"profile=private,domain",
		)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s (%s)", r.proto, firstLine(out, err)))
		}
	}

	if len(failed) == 0 {
		green.Printf("  ✓ Firewall: inbound peer traffic allowed on port %d (TCP + UDP)\n", listenPort)
		return
	}

	color.New(color.FgYellow).Printf(
		"  Note: could not add the firewall rule — %s\n", strings.Join(failed, "; "))
	fmt.Println("        Peers cannot reach you without it, and magnet downloads may fail")
	fmt.Println("        with \"no peers found\" even when the torrent is well seeded.")
	fmt.Println()
	fmt.Println("        Run this once in an Administrator PowerShell:")
	fmt.Printf("          netsh advfirewall firewall add rule name=\"%s\" dir=in action=allow protocol=TCP localport=%d\n",
		winFirewallRuleTCP, listenPort)
	fmt.Printf("          netsh advfirewall firewall add rule name=\"%s\" dir=in action=allow protocol=UDP localport=%d\n",
		winFirewallRuleUDP, listenPort)
	fmt.Println()
	fmt.Println("        Then check it worked with: unarr doctor")
}

// removeWindowsFirewallRules drops what addWindowsFirewallRules created.
// Failures are ignored: an uninstall must not be blocked by a rule that was
// never created (non-elevated install) or already removed by hand.
func removeWindowsFirewallRules() {
	for _, name := range []string{winFirewallRuleTCP, winFirewallRuleUDP} {
		_, _ = svcOutput("netsh", "advfirewall", "firewall", "delete", "rule", "name="+name)
	}
}
