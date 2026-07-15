package cmd

import (
	"net/url"
	"strings"
)

// torrentClawDiscoveryDefaults is the built-in fallback host order for the
// public discovery API, used only when the user's config left no TorrentClaw
// host to route discovery through (e.g. a unarr-only config with the mirror
// list cleared). Kept in sync with config.Default().Auth.Mirrors.
var torrentClawDiscoveryDefaults = []string{
	"https://torrentclaw.to",
	"https://torrentclaw.com",
}

// isUnarrBrandHost reports whether rawURL points at the unarr brand's own
// deployment (unarr.app). That deployment serves only the unarr allow-list of
// /api/v1/* endpoints (mirrors, debrid, stream) and brand-blocks the discovery
// endpoints (search, stats, popular, autocomplete, trending, upcoming,
// collections) with a 404 "ZERO TorrentClaw surface" rule. Discovery must
// therefore never be routed to it.
func isUnarrBrandHost(rawURL string) bool {
	s := strings.TrimSpace(rawURL)
	if s == "" {
		return false
	}
	// A hand-edited scheme-less entry ("unarr.app") parses with an empty Host
	// (everything lands in Path / Opaque) and would slip past the brand filter
	// into the discovery pool as a broken base URL. Give it a scheme first.
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	// TrimSuffix: a rooted FQDN ("unarr.app.") is the same host.
	h := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return h == "unarr.app" || strings.HasSuffix(h, ".unarr.app")
}

// discoveryHosts returns the base URL + failover list for the public discovery
// API client (search / stats / popular / recent / inspect / watch resolve).
//
// Why this exists: the mirror round-tripper rewrites every request's host to the
// pool's current entry, so the discovery client's effective host is the pool
// primary — which defaults to the configured api_url (unarr.app for the unarr
// brand). unarr.app brand-blocks discovery with a 404, and a 404 is NOT transient
// (see agent.IsTransient), so the pool never rolls past it to the TorrentClaw
// mirrors that DO serve discovery. Users get a bare 404 on `unarr search`/`stats`.
//
// Fix: build the discovery pool from the TorrentClaw entries of [apiURL]+mirrors
// (unarr hosts dropped), so discovery always starts on — and fails over between —
// hosts that actually serve it. Falls back to the built-in TorrentClaw defaults
// when the config carries no TorrentClaw host at all.
func discoveryHosts(apiURL string, mirrors []string) (base string, rest []string) {
	var hosts []string
	seen := make(map[string]struct{})

	add := func(raw string) {
		raw = strings.TrimRight(strings.TrimSpace(raw), "/")
		if raw == "" || isUnarrBrandHost(raw) {
			return
		}
		if _, dup := seen[raw]; dup {
			return
		}
		seen[raw] = struct{}{}
		hosts = append(hosts, raw)
	}

	add(apiURL)
	for _, m := range mirrors {
		add(m)
	}

	if len(hosts) == 0 {
		hosts = append([]string(nil), torrentClawDiscoveryDefaults...)
	}

	return hosts[0], hosts[1:]
}
