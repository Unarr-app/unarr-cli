package cmd

import (
	"fmt"
	"strings"

	"github.com/fatih/color"
)

// Windows Defender Firewall blocks inbound connections to a new binary by
// default, and a background scheduled task never gets to show the consent
// dialog a foreground app would. For a BitTorrent client that is not cosmetic:
// without inbound peers a magnet's metadata fetch has far fewer sources, and
// downloads die as "metadata timeout: no peers found".
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
// The rule is scoped BY PROGRAM, not by port. Three reasons, each a bug in the
// port-scoped version this replaced:
//
//   - `Download.ListenPort` is 0 on every stock install (config.Default() never
//     sets it), and the engine maps 0 to a fixed 42069 — so a rule built from
//     the configured value would target port 0 and never be created at all.
//   - When 42069 is busy the engine walks up to 42078 (torrent.go), so a rule
//     pinned to one port silently covers nothing.
//   - A port-scoped rule opens that port for ANY process that binds it.
//
// A program-scoped rule is correct under all three, and is what a well-behaved
// Windows installer does.
const winFirewallRule = "unarr (BitTorrent peers)"

// addWindowsFirewallRules allows inbound peer traffic to the agent binary.
//
// It never fails the install: the rule needs an elevated process, and a
// non-elevated install is still a working agent for everything EXCEPT inbound
// peers. A refusal is reported with the exact command, because the symptom
// otherwise surfaces much later as downloads that "just fail".
//
// binPath is the resolved agent executable (serviceData.BinPath).
func addWindowsFirewallRules(binPath string, green *color.Color) {
	if binPath == "" {
		color.New(color.FgYellow).Println(
			"  Note: could not resolve the agent path, so no firewall rule was added.")
		return
	}

	// Delete first so a re-install after a move/upgrade does not leave a stale
	// rule pointing at the old path. A missing rule exits non-zero — ignored.
	_, _ = svcOutput("netsh", "advfirewall", "firewall", "delete", "rule", "name="+winFirewallRule)

	// No `profile=`: netsh defaults to all profiles. Naming private,domain here
	// would leave the rule inert on Public, which is what Windows assigns to any
	// network it cannot identify — a very common case on laptops.
	out, err := svcOutput("netsh", "advfirewall", "firewall", "add", "rule",
		"name="+winFirewallRule,
		"dir=in",
		"action=allow",
		"program="+binPath,
		"enable=yes",
	)
	if err == nil {
		green.Println("  ✓ Firewall: inbound peer connections allowed for the agent")
		return
	}

	color.New(color.FgYellow).Printf(
		"  Note: could not add the firewall rule — %s\n", firstLine(out, err))
	fmt.Println("        This usually just means the install is not running as Administrator.")
	fmt.Println("        Without the rule, fewer peers can reach you and some downloads may")
	fmt.Println("        fail with \"no peers found\" even on well-seeded torrents.")
	fmt.Println()
	fmt.Println("        To add it, either re-run `unarr daemon install` from an Administrator")
	fmt.Println("        prompt, or paste this into an Administrator PowerShell:")
	fmt.Printf("          netsh advfirewall firewall add rule name=\"%s\" dir=in action=allow program=\"%s\" enable=yes\n",
		winFirewallRule, binPath)
	fmt.Println()
	fmt.Println("        Check it afterwards with: unarr doctor")
}

// windowsFirewallRuleExists reports whether our inbound rule is present.
//
// This is the check that replaced a startup DHT probe. The probe was worse than
// useless here: it is outbound-initiated, and Windows statefully admits the
// solicited reply, so it reported "reachable" on exactly the blocked-inbound
// boxes it was meant to catch — and it opened a clear-net UDP socket, which
// re-introduced the real-IP DHT leak that engine/vpn_wire.go closes on purpose
// (NoDHT = true) for VPN users.
//
// Reading the firewall table costs no network traffic at all, works with the
// tunnel up, and answers the question that actually matters.
func windowsFirewallRuleExists() (bool, error) {
	out, err := svcOutput("netsh", "advfirewall", "firewall", "show", "rule", "name="+winFirewallRule)
	if err != nil {
		// netsh exits non-zero with "No rules match the specified criteria".
		if strings.Contains(strings.ToLower(out), "no rules match") {
			return false, nil
		}
		return false, fmt.Errorf("%s", firstLine(out, err))
	}
	return true, nil
}

// removeWindowsFirewallRules drops what addWindowsFirewallRules created.
// Failures are ignored: an uninstall must not be blocked by a rule that was
// never created (non-elevated install) or already removed by hand.
func removeWindowsFirewallRules() {
	_, _ = svcOutput("netsh", "advfirewall", "firewall", "delete", "rule", "name="+winFirewallRule)
	// The port-scoped rules an older build may have left behind.
	for _, legacy := range []string{"unarr (peer TCP)", "unarr (peer UDP / DHT)"} {
		_, _ = svcOutput("netsh", "advfirewall", "firewall", "delete", "rule", "name="+legacy)
	}
}
