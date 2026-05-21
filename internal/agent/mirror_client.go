package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MirrorEntry mirrors the shape of /api/v1/mirrors items on the server.
type MirrorEntry struct {
	URL     string `json:"url"`
	Label   string `json:"label"`
	Kind    string `json:"kind"`    // "clearnet" | "tor"
	Primary bool   `json:"primary"`
}

// MirrorChannel is an out-of-band status channel (Telegram, status page, etc.)
type MirrorChannel struct {
	URL   string `json:"url"`
	Label string `json:"label"`
}

// MirrorsResponse is the JSON document served by /api/v1/mirrors and
// /api/mirrors.
type MirrorsResponse struct {
	Revision  int             `json:"revision"`
	Mirrors   []MirrorEntry   `json:"mirrors"`
	Tor       *MirrorEntry    `json:"tor"`
	Channels  []MirrorChannel `json:"channels"`
	UpdatedAt string          `json:"updatedAt"`
}

// DefaultStaticFallbackURLs lists off-domain JSON copies of the mirror list.
// Hard-coded here (not loaded from config) because the whole point is to
// have something to consult when config-driven URLs all fail.
//
// Hosted on IPFS (content-addressed, re-pinnable, no host can take it down
// permanently — same bytes re-pinned anywhere keep the same CID). Multiple
// public gateways are listed so a single gateway being blocked doesn't kill
// the fallback; the /ipfs/<CID>/ path is identical across all gateways.
//
// GitHub Pages was removed 2026-05-17: the whole torrentclaw org is
// shadow-banned (public repos 404 to anonymous users). Do NOT re-add any
// github.io URL. Keep this slice in sync with `STATIC_FALLBACKS` in
// `torrentclaw-web/src/lib/mirrors-config.ts` — when the IPFS CID changes
// (scripts/publish-mirrors-ipfs.sh), update both.
//
// Future hardening: sign mirrors.json with the same ed25519 release key
// (or a sibling) so a hijack of any single static host cannot serve a
// malicious mirror list. Today the only signal is "agreement between
// independent providers" via cross-checking, which we leave to the
// operator.
const mirrorsIPFSCID = "bafybeigwux74fek7uky7nct47z5eqwwnpylakfxppqqnzbuxdw7p3ikfdy"

var DefaultStaticFallbackURLs = []string{
	"https://ipfs.io/ipfs/" + mirrorsIPFSCID + "/mirrors.json",
	"https://dweb.link/ipfs/" + mirrorsIPFSCID + "/mirrors.json",
	"https://gateway.pinata.cloud/ipfs/" + mirrorsIPFSCID + "/mirrors.json",
}

// FetchMirrorsWithFallback pulls the mirror list using FetchMirrors against
// `candidates` first; if every candidate fails, it falls back to the static
// JSON copies on off-domain hosts (GitHub Pages, Cloudflare Pages, …).
//
// This is the function `unarr mirrors update` should call when it wants the
// strongest "give me a working mirror list no matter what" guarantee.
func FetchMirrorsWithFallback(ctx context.Context, candidates []string, userAgent string) (*MirrorsResponse, error) {
	resp, err := FetchMirrors(ctx, candidates, userAgent)
	if err == nil {
		return resp, nil
	}
	if len(DefaultStaticFallbackURLs) == 0 {
		return nil, err
	}
	// Try the static JSON files directly. They follow the same wire shape so
	// we can reuse the same parser — but the URLs already include the JSON
	// suffix so we hit them with `fetchMirrorsJSON` instead of FetchMirrors
	// (which appends /api/v1/mirrors).
	staticResp, staticErr := fetchMirrorsJSON(ctx, DefaultStaticFallbackURLs, userAgent)
	if staticErr == nil {
		return staticResp, nil
	}
	return nil, fmt.Errorf("primary failed (%v) and static fallback failed (%v)", err, staticErr)
}

// fetchMirrorsJSON pulls a MirrorsResponse from already-fully-qualified URLs
// (e.g. https://ipfs.io/ipfs/<CID>/mirrors.json). Each candidate is tried
// in order; the first success wins.
func fetchMirrorsJSON(ctx context.Context, urls []string, userAgent string) (*MirrorsResponse, error) {
	if len(urls) == 0 {
		return nil, fmt.Errorf("no static fallback URLs configured")
	}
	hc := &http.Client{Timeout: 15 * time.Second}
	var lastErr error
	for _, url := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		if userAgent != "" {
			req.Header.Set("User-Agent", userAgent)
		}
		req.Header.Set("Accept", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("%s returned HTTP %d", url, resp.StatusCode)
			continue
		}
		var out MirrorsResponse
		if err := json.Unmarshal(body, &out); err != nil {
			lastErr = fmt.Errorf("%s: invalid JSON: %w", url, err)
			continue
		}
		if len(out.Mirrors) == 0 {
			lastErr = fmt.Errorf("%s returned empty mirror list", url)
			continue
		}
		return &out, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no reachable static fallback")
	}
	return nil, lastErr
}

// FetchMirrors pulls the latest mirror list from the server.
//
// The endpoint is intentionally public and unauthenticated: the whole point
// of mirror discovery is that it must work even when the user's API key
// is invalid, expired, or the auth path is unreachable. The function tries
// each candidate base URL in order so a takedown of the primary doesn't
// also kill mirror discovery.
func FetchMirrors(ctx context.Context, candidates []string, userAgent string) (*MirrorsResponse, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no mirror discovery URLs configured")
	}

	hc := &http.Client{Timeout: 15 * time.Second}

	var lastErr error
	for _, base := range candidates {
		if base == "" {
			continue
		}
		url := base + "/api/v1/mirrors"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		if userAgent != "" {
			req.Header.Set("User-Agent", userAgent)
		}
		req.Header.Set("Accept", "application/json")

		resp, err := hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("%s returned HTTP %d", base, resp.StatusCode)
			continue
		}
		var out MirrorsResponse
		if err := json.Unmarshal(body, &out); err != nil {
			lastErr = fmt.Errorf("%s: invalid JSON: %w", base, err)
			continue
		}
		if len(out.Mirrors) == 0 {
			lastErr = fmt.Errorf("%s returned empty mirror list", base)
			continue
		}
		return &out, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no reachable mirror discovery endpoint")
	}
	return nil, fmt.Errorf("fetch mirrors: %w", lastErr)
}

// ToConfig splits a MirrorsResponse into (primary, extras) suitable for
// rebuilding a MirrorPool or persisting back into config.toml.
//
// The "primary" returned here is whichever entry has primary=true. If none
// are flagged, the first one wins.
func (m *MirrorsResponse) ToConfig() (primary string, extras []string) {
	if m == nil {
		return "", nil
	}
	var picked *MirrorEntry
	for i := range m.Mirrors {
		if m.Mirrors[i].Primary {
			picked = &m.Mirrors[i]
			break
		}
	}
	if picked == nil && len(m.Mirrors) > 0 {
		picked = &m.Mirrors[0]
	}
	if picked != nil {
		primary = picked.URL
	}
	for _, e := range m.Mirrors {
		if e.URL == primary {
			continue
		}
		extras = append(extras, e.URL)
	}
	return primary, extras
}
