package engine

import (
	"context"
	"log"
	"time"
)

// shouldAutoPublishHTTPS decides whether to keep a WAN mapping for the TLS
// listener. Three inputs, and the order they are read in is the policy:
//
//   - autoHTTPSUpnp is the operator's veto. `auto_https_upnp = false` suppresses
//     the mapping outright, including the one enable_upnp used to make.
//   - requireToken is what makes the default-on exposure defensible: every
//     request to the TLS listener carries a mandatory per-request token on top of
//     TLS, so publishing it buys a stable direct-TLS host without opening
//     anything unauthenticated.
//   - enableUPnP is the legacy explicit opt-in, and it still stands on its own —
//     an operator who turned it on with the token disabled asked for the exposure
//     knowingly, and this must not silently revoke that.
//
// Note what is NOT here: the WebDAV mount. /dav/ shares this listener's mux, but
// it enforces its own local-network guard (see remoteIsLocalNetwork), so
// publishing the port exposes playback, not the library.
func (ss *StreamServer) shouldAutoPublishHTTPS() bool {
	if !ss.autoHTTPSUpnp {
		return false
	}
	return ss.requireToken || ss.enableUPnP
}

// maintainHTTPSPortMapping publishes the HTTPS stream port to the WAN via
// UPnP/NAT-PMP and renews the lease before it expires, so the per-agent
// direct-TLS host (https://<wanip>.<hash>.agent.unarr.app:<port>) stays reachable
// for remote browsers without a manual port-forward. Best-effort and
// self-healing: it retries when the gateway is absent or a lease drops, so a
// router that appears later (or a renewed 2h lease) is still picked up. Bound to
// the daemon context; the mapping is torn down in Shutdown. Runs in its own
// goroutine — SetupUPnP does blocking SSDP discovery we must not run inline.
func (ss *StreamServer) maintainHTTPSPortMapping(ctx context.Context) {
	r := &httpsMappingRenewer{ss: ss}
	r.refresh()
	t := time.NewTicker(httpsMappingRenewEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.refresh()
		}
	}
}

// httpsMappingRenewEvery sits comfortably inside the 2h lease SetupUPnP asks for,
// so a renewal that fails still has two more attempts before the mapping lapses.
const httpsMappingRenewEvery = 50 * time.Minute

// httpsMappingRenewer holds one maintainer goroutine's view of the WAN mapping.
// prevMapped is deliberately goroutine-local rather than a field on StreamServer:
// it exists only to detect transitions for the callback, and keeping it out of
// the shared struct means it needs no lock and cannot be read by anyone else.
type httpsMappingRenewer struct {
	ss         *StreamServer
	prevMapped bool
}

// set records the mapping and its usable state under the lock, then fires the
// change callback OUTSIDE the lock and only on a transition — the daemon writes
// state on every call, so firing per tick would mean a disk write every 50
// minutes forever.
func (r *httpsMappingRenewer) set(m *UPnPMapping, mapped bool) {
	r.ss.mu.Lock()
	r.ss.httpsUpnpMapping = m
	r.ss.httpsWanMapped = mapped
	r.ss.mu.Unlock()
	if mapped == r.prevMapped {
		return
	}
	r.prevMapped = mapped
	if r.ss.wanMappedCB != nil {
		r.ss.wanMappedCB(mapped)
	}
}

// refresh (re)publishes the port and reconciles the reported state with what the
// gateway actually did.
func (r *httpsMappingRenewer) refresh() {
	m, err := SetupUPnP(r.ss.httpsPort)
	if err != nil {
		r.handleFailure(err)
		return
	}
	r.handleMapping(m)
}

// handleFailure reacts to a gateway that refused or vanished. Only interesting
// after a prior success: clearing a mapping we never had would fire a spurious
// callback, and a router that simply isn't there is the normal case on a LAN
// without UPnP.
func (r *httpsMappingRenewer) handleFailure(err error) {
	if !r.prevMapped {
		return
	}
	// The lease dropped or the gateway went away AFTER we had it. Clear the
	// mapping so the web stops preferring a now-dead direct-TLS host and falls
	// back to the funnel; the next tick retries (self-heal).
	log.Printf("[stream] HTTPS UPnP renewal failed: %v — clearing WAN mapping (funnel fallback)", err)
	r.set(nil, false)
}

// handleMapping accepts a mapping only if it is actually usable. A successful
// AddPortMapping means "the gateway accepted it", not "it does what we asked":
// some gateways reassign the external port, and the web builds the direct-TLS
// host from the INTERNAL port, so a mismatch advertises a URL nothing answers.
func (r *httpsMappingRenewer) handleMapping(m *UPnPMapping) {
	if m.ExternalPort != r.ss.httpsPort {
		log.Printf("[stream] HTTPS UPnP external port %d != internal %d — not advertising direct-TLS (funnel fallback)",
			m.ExternalPort, r.ss.httpsPort)
		m.Remove() // drop it now, or every tick leaks another
		r.set(nil, false)
		return
	}
	if !r.prevMapped {
		log.Printf("[stream] HTTPS UPnP: published port %d to WAN via %s:%d (auto, renewing every %s)",
			r.ss.httpsPort, m.ExternalIP, m.ExternalPort, httpsMappingRenewEvery)
	}
	r.set(m, true)
}
