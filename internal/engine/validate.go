// Package engine — validate.go centralises input validators used by the
// stream/HLS HTTP handlers and the daemon glue. Keep new validators in this
// file so a future reviewer can audit the trust boundary in one place.
package engine

import "regexp"

// validSessionID restricts session IDs to characters safe for use as a single
// filesystem path component. Server-issued UUIDs and hex strings match this;
// anything containing slashes, dots, or path separators is rejected so a
// compromised or buggy server cannot escape hlsTmpDirRoot via os.MkdirAll.
var validSessionID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// defaultCORSAllowedOrigins is the baseline of browser origins that may
// XHR-probe `/health` and friends on the local daemon. Production hosts are
// hardcoded; localhost on the dev port used by torrentclaw-web is included
// so dev builds work without extra configuration. Operators may add more
// origins via the [downloads] cors_extra_origins TOML key.
//
// The dev port matches `next dev -p 3030` in torrentclaw-web/package.json.
// 127.0.0.1 is listed in addition to localhost because some browsers treat
// them as distinct origins for CORS.
//
// Mirrors (`.to`, `staging.torrentclaw.com`, `www.`) are listed so a user
// playing from any official mirror succeeds the HEAD probe; without these
// the browser drops the response for "missing ACAO" and the player reports
// "404 todos los canales" even though the daemon returned 200.
//
// Note: media tags (<video src>, <audio src>) do not send the Origin
// header so they are not gated by CORS at all; this allowlist only
// affects fetch()/XHR.
var defaultCORSAllowedOrigins = []string{
	"https://torrentclaw.com",
	"https://www.torrentclaw.com",
	"https://app.torrentclaw.com",
	"https://staging.torrentclaw.com",
	"https://torrentclaw.to",
	"https://www.torrentclaw.to",
	// unarr brand (separate deployment). The web player + agent endpoints run
	// under unarr.app; without these the browser drops every /hls + /stream
	// response (no Access-Control-Allow-Origin) and playback fails on unarr.
	"https://unarr.app",
	"https://www.unarr.app",
	// Tor mirror — Tor Browser sends `Origin: http://<addr>.onion` (plain
	// http, no port). Mirror address is the BUILT_IN_ONION constant from
	// torrentclaw-web/src/lib/mirrors-config.ts; rotates rarely, kept in
	// sync by hand. Daemon also dynamically merges /api/mirrors at startup
	// (see daemon.go) so a new key doesn't need a CLI rebuild.
	"http://torrentf3aifidcsaaanmnmuhv2s53r6hqsl3zkmfidiaxainkeqk5id.onion",
	"http://localhost:3030",
	"http://127.0.0.1:3030",
	// unarr brand dev server (`pnpm dev:unarr`, port 3029). The unarr web player
	// runs here locally; without these the browser drops every /hls + /stream
	// response and playback fails on the unarr dev page (mirrors the 3030 pair).
	"http://localhost:3029",
	"http://127.0.0.1:3029",
}

// buildCORSAllowlist merges the default origins with any extras supplied by
// the operator. Returned map is intended to be installed once at Listen()
// and treated as read-only afterwards.
func buildCORSAllowlist(extra []string) map[string]struct{} {
	out := make(map[string]struct{}, len(defaultCORSAllowedOrigins)+len(extra))
	for _, o := range defaultCORSAllowedOrigins {
		out[o] = struct{}{}
	}
	for _, o := range extra {
		if o != "" {
			out[o] = struct{}{}
		}
	}
	return out
}
