package engine

import (
	"net/http"
	"testing"
)

func TestRemoteIsLocalNetwork(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:5000", true},
		{"[::1]:5000", true},
		{"[::ffff:127.0.0.1]:5000", true}, // Linux dual-stack form
		{"192.168.1.20:5000", true},
		{"10.0.0.7:5000", true},
		{"172.16.4.1:5000", true},
		{"[fd7a:115c:a1e0::1]:5000", true}, // IPv6 ULA
		{"169.254.10.1:5000", true},        // link-local
		{"100.101.102.103:5000", true},     // Tailscale CGNAT
		{"8.8.8.8:5000", false},
		{"[2606:4700::1111]:5000", false},
		{"172.32.0.1:5000", false},  // just outside 172.16/12
		{"100.128.0.1:5000", false}, // just outside 100.64/10
		{"192.168.1.20", true},      // bare address, no port
		{"", false},                 // fail closed
		{"not-an-address:5000", false},
	}
	for _, c := range cases {
		if got := remoteIsLocalNetwork(c.addr); got != c.want {
			t.Errorf("remoteIsLocalNetwork(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

// The stream port reaches the public internet whenever it is published, and
// /dav/ rides the same mux. Its Basic auth has no rate limiting, so a WAN caller
// must not get as far as a password prompt — even holding valid credentials.
func TestWebDAVRejectsWANByDefault(t *testing.T) {
	_, h := newWebDAVServer(t)

	if got := davRequestFromAddr(t, h, "PROPFIND", "/dav/", testDAVWANAddr, true).Code; got != http.StatusNotFound {
		t.Errorf("an authenticated WAN caller must get 404, got %d", got)
	}
	if got := davRequestFromAddr(t, h, "GET", "/dav/movie.mkv", testDAVWANAddr, true).Code; got != http.StatusNotFound {
		t.Errorf("a WAN caller must not be able to fetch media, got %d", got)
	}
}

// 404 rather than 401: a 401 would confirm a WebDAV mount lives here and hand a
// scanner the realm plus a login oracle to grind against.
func TestWebDAVDoesNotAnnounceItselfToWAN(t *testing.T) {
	_, h := newWebDAVServer(t)
	rec := davRequestFromAddr(t, h, "PROPFIND", "/dav/", testDAVWANAddr, false)

	if rec.Code == http.StatusUnauthorized {
		t.Error("401 tells a scanner the mount exists; the guard must 404 instead")
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("no auth challenge may leak to the WAN, got %q", got)
	}
}

// OPTIONS is answered before auth for capability discovery, so it needs the same
// gate — otherwise it becomes the fingerprinting path the 404 was meant to close.
func TestWebDAVOptionsIsGatedForWAN(t *testing.T) {
	_, h := newWebDAVServer(t)

	if got := davRequestFromAddr(t, h, http.MethodOptions, "/dav/", testDAVWANAddr, false).Code; got != http.StatusNotFound {
		t.Errorf("OPTIONS from the WAN must 404, got %d", got)
	}
	if got := davRequestFromAddr(t, h, http.MethodOptions, "/dav/", testDAVLANAddr, false).Code; got != http.StatusOK {
		t.Errorf("OPTIONS from the LAN must still work, got %d", got)
	}
}

// The guard must not cost local users anything: LAN and Tailscale callers keep
// the exact behaviour they had before it existed.
func TestWebDAVStillServesLocalNetwork(t *testing.T) {
	_, h := newWebDAVServer(t)

	if got := davRequestFromAddr(t, h, "GET", "/dav/movie.mkv", testDAVLANAddr, true).Code; got != http.StatusOK {
		t.Errorf("a LAN caller must reach the mount, got %d", got)
	}
	if got := davRequestFromAddr(t, h, "GET", "/dav/movie.mkv", "100.101.102.103:5000", true).Code; got != http.StatusOK {
		t.Errorf("a Tailscale caller must reach the mount, got %d", got)
	}
	// Local but unauthenticated still fails on auth, not on the network gate.
	if got := davRequestFromAddr(t, h, "PROPFIND", "/dav/", testDAVLANAddr, false).Code; got != http.StatusUnauthorized {
		t.Errorf("a LAN caller without credentials must get 401, got %d", got)
	}
}

// Opting in restores WAN reach — with auth still enforced, which is the whole
// point of it being an explicit flag rather than a silent default.
func TestWebDAVAllowWANOptIn(t *testing.T) {
	ss, h := newWebDAVServer(t)
	ss.SetWebDAVAllowWAN(true)

	if got := davRequestFromAddr(t, h, "GET", "/dav/movie.mkv", testDAVWANAddr, true).Code; got != http.StatusOK {
		t.Errorf("webdav_allow_wan must let an authenticated WAN caller through, got %d", got)
	}
	if got := davRequestFromAddr(t, h, "PROPFIND", "/dav/", testDAVWANAddr, false).Code; got != http.StatusUnauthorized {
		t.Errorf("opting into WAN must not drop Basic auth, got %d", got)
	}
}

// The read-only contract is enforced before the network gate is even relevant,
// but a mutating verb from the WAN must still not reveal the mount.
func TestWebDAVWriteVerbFromWANIs404NotMethodNotAllowed(t *testing.T) {
	_, h := newWebDAVServer(t)

	if got := davRequestFromAddr(t, h, http.MethodPut, "/dav/movie.mkv", testDAVWANAddr, true).Code; got != http.StatusNotFound {
		t.Errorf("a WAN write attempt must 404 (not 405, which confirms the mount), got %d", got)
	}
	if got := davRequestFromAddr(t, h, http.MethodPut, "/dav/movie.mkv", testDAVLANAddr, true).Code; got != http.StatusMethodNotAllowed {
		t.Errorf("a LAN write attempt must still be 405, got %d", got)
	}
}
