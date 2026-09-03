package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The daemon startup path must not open a clear-net socket to diagnose peer
// reachability.
//
// An earlier version of this check probed the global DHT at every daemon start.
// It was wrong twice: the probe is outbound-initiated, so Windows statefully
// admits the reply and it reported "reachable" on exactly the blocked-inbound
// machines it was meant to catch; and it bound a clear-net UDP socket, which
// re-introduced the real-IP DHT leak engine/vpn_wire.go closes on purpose
// (NoDHT = true) — including in the fail-closed VPN state where P2P is off.
//
// This test anchors the invariant at the call site rather than the
// implementation: it asserts the daemon startup does not reach for probeDHT.
// If the startup call is renamed or moved, point this at the new site — do not
// delete the assertion.
func TestDaemonStartupDoesNotProbeTheDHT(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("daemon.go"))
	if err != nil {
		t.Fatalf("read daemon.go: %v", err)
	}
	if strings.Contains(string(src), "probeDHT") {
		t.Error("daemon.go calls probeDHT: the startup path must not open a clear-net " +
			"UDP socket — it leaks the real IP for VPN users (see engine/vpn_wire.go) " +
			"and cannot observe a blocked INBOUND port anyway")
	}
	if !strings.Contains(string(src), "logInboundPeerReachability") {
		t.Error("daemon.go no longer reports inbound peer reachability at startup; " +
			"without it a firewalled Windows agent fails downloads silently")
	}
}

// Outside Windows the check must cost nothing at all — no netsh, no network, no
// log line. It is wired unconditionally into the daemon startup, so a
// non-Windows agent would otherwise pay for a diagnosis that cannot apply.
func TestInboundReachabilityIsAWindowsOnlyNoOp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this asserts the non-Windows no-op")
	}
	// Must return promptly and without panicking. A netsh invocation on Linux
	// would either block on PATH lookup or error; neither is acceptable here.
	done := make(chan struct{})
	go func() {
		defer close(done)
		logInboundPeerReachability()
	}()
	<-done
}
