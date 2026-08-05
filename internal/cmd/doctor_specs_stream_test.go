package cmd

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

// listenOn takes a real port and holds it for the test, returning the port
// number. The listener is NOT an unarr stream server, which is the point: it
// stands in for the foreign process this check has to distinguish from us.
func listenOn(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// freePort finds a port nothing is on. Inherently a small race — something
// could take it between the close and the check — but nothing else in this
// process will, and the alternative is not testing the free path at all.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func TestStreamPortResultDisabled(t *testing.T) {
	msg, err := streamPortResult(0, "stream_port")
	if err != nil {
		t.Fatalf("a disabled port is not a failure: %v", err)
	}
	if !strings.Contains(msg, "disabled") {
		t.Errorf("message = %q", msg)
	}
}

// The failure this check exists for: something that is not unarr owns the port,
// so the daemon can register and report healthy while serving nothing.
func TestStreamPortResultFailsWhenAForeignProcessHoldsIt(t *testing.T) {
	withDataDir(t) // no state file -> daemonIsAlive() is false
	port := listenOn(t)

	msg, err := streamPortResult(port, "stream_port")
	if err == nil {
		t.Fatalf("a port held by a foreign process must FAIL, got %q", msg)
	}
	if !strings.Contains(msg, fmt.Sprint(port)) {
		t.Errorf("message does not name the port: %q", msg)
	}
	// The remedy has to be actionable without a second support round-trip.
	if !strings.Contains(msg, "ss -ltnp") && !strings.Contains(msg, "netstat") {
		t.Errorf("message does not say how to find the holder: %q", msg)
	}
}

func TestStreamPortResultPassesWhenFreeAndNoDaemon(t *testing.T) {
	withDataDir(t)
	msg, err := streamPortResult(freePort(t), "stream_port")
	if err != nil {
		t.Fatalf("a free port with no daemon is not a failure: %v", err)
	}
	if strings.HasPrefix(msg, "!") {
		t.Errorf("nor is it a warning: %q", msg)
	}
}

// The HTTPS listener only starts once a certificate exists, so an unbound
// https_stream_port is the normal state on most installs. It must never be red.
func TestHTTPSStreamPortIsNeverAFailure(t *testing.T) {
	withDataDir(t)
	for _, port := range []int{0, freePort(t), listenOn(t)} {
		msg, err := httpsStreamPortResult(port)
		if err != nil {
			t.Errorf("port %d: HTTPS must never FAIL, got error %v (%q)", port, err, msg)
		}
	}
}

func TestLANReachabilityWarnsWithoutADaemon(t *testing.T) {
	withDataDir(t)
	msg, err := lanReachabilityResult(11818)
	if err != nil {
		t.Fatalf("no daemon is not a failure of the LAN check: %v", err)
	}
	if !strings.HasPrefix(msg, "!") {
		t.Errorf("expected a WARN, got %q", msg)
	}
}

func TestIsAddrInUse(t *testing.T) {
	// The real thing, rather than a hand-written string: the message Go builds
	// is what this function has to recognise, and it differs per platform.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	_, dialErr := net.Listen("tcp", ln.Addr().String())
	if dialErr == nil {
		t.Skip("this platform allows rebinding the same address")
	}
	if !isAddrInUse(dialErr) {
		t.Errorf("did not recognise the real EADDRINUSE for this platform: %q", dialErr)
	}

	if isAddrInUse(nil) {
		t.Error("nil is not an in-use error")
	}
	if isAddrInUse(errors.New("permission denied")) {
		t.Error("a permission error was misread as in-use — it must be reported, not swallowed")
	}
}
