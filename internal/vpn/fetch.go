// Config fetch for the MANAGED VPN mode: the agent asks the web for the
// WireGuard .conf tied to its account (/api/internal/agent/vpn-config).
//
// Split out of vpn.go: talking to the web API — auth, status-code semantics,
// and turning a response into an error a human can act on — is a separate
// responsibility from bringing up and supervising the userspace tunnel, which
// is what the rest of the package does.
package vpn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"
)

// ErrCode classifies fetch failures the agent should react to differently.
type ErrCode string

const (
	ErrDisabled       ErrCode = "disabled"        // 503 — VPN feature off server-side
	ErrNotProvisioned ErrCode = "not_provisioned" // 403 — user has no active VPN
	ErrSlotOnDevice   ErrCode = "slot_on_device"  // 409 — slot claimed by a device
	ErrUpstream       ErrCode = "upstream"        // network / 5xx / parse
)

// FetchError carries an ErrCode so callers can decide whether to retry, warn, or
// fall back to a clear (non-VPN) download.
//
// Host is the API host the fetch was aimed at. It is part of the error because a
// server-side "off" answer is only meaningful together with WHICH server said it:
// the agent defaults to unarr.app but historically fell back to torrentclaw.com
// in three places, and a deployment that simply lacks the VPN env answers 503 for
// every account. Without the host the user reads "VPN disabled server-side" and
// concludes their paid add-on is broken (it isn't) — a real support case.
type FetchError struct {
	Code ErrCode
	Msg  string
	Host string
}

func (e *FetchError) Error() string {
	if e.Host != "" {
		return fmt.Sprintf("vpn fetch: %s (%s) at %s", e.Msg, e.Code, e.Host)
	}
	return fmt.Sprintf("vpn fetch: %s (%s)", e.Msg, e.Code)
}

type fetchResponse struct {
	Content  string `json:"content"`
	Filename string `json:"filename"`
	ServerID int    `json:"serverId"`
	Mode     string `json:"mode"`
	Error    string `json:"error"`
	CodeStr  string `json:"code"`
}

// FetchRequest is the input to FetchConfig. A struct rather than a parameter
// list because four of the fields are bare strings in a row — APIURL, APIKey,
// UserAgent, AgentID — and a caller that transposes two of them compiles
// cleanly and fails at runtime as an auth error.
type FetchRequest struct {
	APIURL    string // API base, e.g. "https://unarr.app"
	APIKey    string // agent API key, sent as `Authorization: Bearer <key>`
	UserAgent string
	AgentID   string // lets the web arbitrate the single WireGuard slot; "" omits it
	// Probe validates provisioning WITHOUT claiming the WireGuard slot, so
	// `unarr vpn status --check` never steals the slot from the real owner.
	Probe bool
}

// FetchConfig retrieves the agent's WireGuard .conf from the web API. Auth is
// `Authorization: Bearer <apiKey>` (the agent-auth scheme). AgentID lets the web
// arbitrate the single WireGuard slot (first agent to ask claims it; others get
// 409 → ErrSlotOnDevice and should use OpenVPN on their host instead).
func FetchConfig(ctx context.Context, r FetchRequest) (string, error) {
	q := neturl.Values{}
	if r.AgentID != "" {
		q.Set("agentId", r.AgentID)
	}
	if r.Probe {
		q.Set("probe", "1")
	}
	base := strings.TrimSuffix(r.APIURL, "/")
	host := hostLabel(base)
	url := base + "/api/internal/agent/vpn-config"
	if len(q) > 0 {
		url += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", &FetchError{ErrUpstream, err.Error(), host}
	}
	req.Header.Set("Authorization", "Bearer "+r.APIKey)
	req.Header.Set("User-Agent", r.UserAgent)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", &FetchError{ErrUpstream, err.Error(), host}
	}
	defer resp.Body.Close()

	var body fetchResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)

	switch resp.StatusCode {
	case http.StatusOK:
		if body.Content == "" {
			return "", &FetchError{ErrUpstream, "empty config", host}
		}
		return body.Content, nil
	case http.StatusServiceUnavailable:
		// A 503 is only ErrDisabled when it came from the APPLICATION — the
		// handler answers a JSON body when the VPN feature is off. A bare 503
		// (no JSON error) is an edge/proxy/deploy blip: transient, and telling
		// the user their VPN is "disabled server-side" for it is actively
		// misleading. Classify those as upstream so the caller retries instead.
		if body.Error == "" {
			return "", &FetchError{ErrUpstream, "server temporarily unavailable (503)", host}
		}
		return "", &FetchError{ErrDisabled, "the VPN feature is not enabled on this server", host}
	case http.StatusForbidden:
		return "", &FetchError{ErrNotProvisioned, "no active VPN for this account", host}
	case http.StatusConflict:
		return "", &FetchError{ErrSlotOnDevice, "VPN slot is active on one of your devices", host}
	default:
		msg := body.Error
		if msg == "" {
			msg = "unexpected status " + strconv.Itoa(resp.StatusCode)
		}
		return "", &FetchError{ErrUpstream, msg, host}
	}
}

// hostLabel reduces an API base URL to its host for user-facing messages
// ("https://unarr.app" → "unarr.app"). Falls back to the raw string when it
// doesn't parse, so an odd config still yields a usable message.
func hostLabel(base string) string {
	u, err := neturl.Parse(base)
	if err != nil || u.Host == "" {
		return base
	}
	return u.Host
}
