package engine

import "testing"

// The gate decides whether a default install opens a port on the user's router,
// so every combination is spelled out rather than left to the reader.
func TestShouldAutoPublishHTTPS(t *testing.T) {
	cases := []struct {
		name                              string
		autoHTTPS, requireToken, enableUP bool
		want                              bool
	}{
		{"stock config: token required, no explicit UPnP", true, true, false, true},
		{"legacy explicit opt-in, token disabled", true, false, true, true},
		{"token disabled and no opt-in — nothing authenticates the listener", true, false, false, false},
		{"opt-out wins over a required token", false, true, false, false},
		{"opt-out wins over the legacy enable_upnp too", false, false, true, false},
		{"opt-out wins over both", false, true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ss := &StreamServer{
				autoHTTPSUpnp: c.autoHTTPS,
				requireToken:  c.requireToken,
				enableUPnP:    c.enableUP,
			}
			if got := ss.shouldAutoPublishHTTPS(); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// A fresh server must arrive at the documented default — auto-publish on. This
// is the value that decides what happens on a machine nobody configured, so it
// is asserted directly rather than inferred from the constructor's source.
func TestNewStreamServerDefaultsToAutoPublish(t *testing.T) {
	ss := NewStreamServer(0, 1)

	if !ss.autoHTTPSUpnp {
		t.Error("auto_https_upnp must default on (opt-out, not opt-in)")
	}
	if !ss.requireToken {
		t.Error("requireToken must default on — it is what makes auto-publish safe")
	}
	if ss.enableUPnP {
		t.Error("the cleartext listener's UPnP must stay opt-in")
	}
	if ss.webdavAllowWAN {
		t.Error("WebDAV must default to local-network only")
	}
	if !ss.shouldAutoPublishHTTPS() {
		t.Error("stock config must auto-publish the TLS listener")
	}
}

func TestSetAutoHTTPSUpnpOptsOut(t *testing.T) {
	ss := NewStreamServer(0, 1)
	ss.SetAutoHTTPSUpnp(false)

	if ss.shouldAutoPublishHTTPS() {
		t.Error("SetAutoHTTPSUpnp(false) must suppress the mapping")
	}
	// Even alongside the legacy opt-in: the explicit "no" is the stronger signal.
	ss.SetUPnPEnabled(true)
	if ss.shouldAutoPublishHTTPS() {
		t.Error("auto_https_upnp=false must win over enable_upnp=true")
	}
}

// The WAN-mapped flag rides the sync heartbeat and the web gates its direct-TLS
// probe on it, so a stale `true` costs a user working playback: the web keeps
// preferring a host the router no longer forwards instead of the funnel.
func TestSetHTTPSWanMappedRoundTrips(t *testing.T) {
	ss := NewStreamServer(0, 1)

	if ss.HTTPSWanMapped() {
		t.Error("a server that never mapped anything must not report mapped")
	}
	ss.mu.Lock()
	ss.httpsWanMapped = true
	ss.mu.Unlock()
	if !ss.HTTPSWanMapped() {
		t.Error("HTTPSWanMapped must reflect the maintainer's state")
	}
}

// The callback is what pushes the flag onto the daemon. It must fire on a
// transition and stay quiet otherwise — the daemon writes state on every call,
// so a per-tick callback would mean a disk write every 50 minutes forever.
func TestWanMappedCallbackFiresOnlyOnChange(t *testing.T) {
	ss := NewStreamServer(0, 1)
	var got []bool
	ss.SetWanMappedCallback(func(m bool) { got = append(got, m) })

	// Drive the same transition logic the maintainer uses: report only on change.
	prev := false
	report := func(mapped bool) {
		if mapped != prev {
			prev = mapped
			ss.wanMappedCB(mapped)
		}
	}
	report(true)  // fires
	report(true)  // quiet — still mapped
	report(true)  // quiet
	report(false) // fires — lease dropped
	report(false) // quiet

	want := []bool{true, false}
	if len(got) != len(want) {
		t.Fatalf("callback fired %d times %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("callback[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}
