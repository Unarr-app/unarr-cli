package cmd

// Liveness of a RUNNING CloudFlare Quick Tunnel.
//
// The supervisor in funnel_supervise.go only ever learned of a dead tunnel by
// the cloudflared PROCESS exiting. That misses the failure seen in the field:
// the tunnel is reclaimed at the CF edge (hostname first answers a 530 error
// page, then goes NXDOMAIN) while cloudflared stays alive, retrying its edge
// connections forever. The daemon kept reporting the dead hostname on every
// heartbeat, the web kept building subtitle/thumbnail URLs on it, and nothing
// anywhere said a word. This watcher closes that gap by asking the two
// questions that actually distinguish "up" from "zombie":
//
//   - does the public hostname still resolve? (NXDOMAIN = reclaimed)
//   - does GET https://<hostname>/health, through CF, answer with OUR daemon's
//     JSON? (a 530 page is CF speaking for a tunnel it cannot reach)
//
// plus, when cloudflared's metrics port is known, its own /ready count as a
// cheap local hint (0 edge connections). A verdict takes failLimit CONSECUTIVE
// failures — a flaky resolver or a CF blip must not restart a healthy tunnel —
// and only after the tunnel has lived minLifetime, so a freshly-provisioned
// hostname that has not propagated yet is never mistaken for a dead one.
//
// What deliberately does NOT count as a failure: SERVFAIL / resolver timeouts
// (that is our DNS, not the tunnel), and HTTP 429 (CF rate-limiting quick
// tunnels is a throttle, not a death).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	funnelHealthEvery = 30 * time.Second
	// funnelHealthFailLimit consecutive failures = dead. With one probe round
	// every funnelHealthEvery that is 60-90 s from death to verdict.
	funnelHealthFailLimit = 3
	// funnelHealthMinLifetime: no verdict before the tunnel is this old. A new
	// hostname can take a minute to propagate through CF's own DNS.
	funnelHealthMinLifetime = 2 * time.Minute
	funnelHealthHTTPTimeout = 10 * time.Second
)

// probeOutcome is what one probe round concluded about the tunnel.
type probeOutcome int

const (
	probeHealthy probeOutcome = iota // the daemon answered through the tunnel
	probeFailed                      // dead-shaped: NXDOMAIN, 5xx page, timeout
	probeIgnored                     // says nothing: SERVFAIL, 429, no metrics
)

// funnelProber is the slice of *funnel.Tunnel the watcher needs (test seam).
type funnelProber interface {
	Ready(ctx context.Context) (int, error)
}

// funnelHealth is one watcher's policy + dependencies. Zero values are not
// usable; build with newFunnelHealth (production) or by hand in tests.
type funnelHealth struct {
	every       time.Duration
	failLimit   int
	minLifetime time.Duration
	// get performs the end-to-end HTTP probe; returns (outcome, detail).
	get func(ctx context.Context, healthURL string) (probeOutcome, string)
	// lookup resolves the hostname; returns (outcome, detail).
	lookup func(ctx context.Context, host string) (probeOutcome, string)
	prober funnelProber
}

func newFunnelHealth(prober funnelProber) *funnelHealth {
	return &funnelHealth{
		every:       funnelHealthEvery,
		failLimit:   funnelHealthFailLimit,
		minLifetime: funnelHealthMinLifetime,
		get:         httpHealthProbe(&http.Client{Timeout: funnelHealthHTTPTimeout}),
		lookup:      dnsHealthProbe(net.DefaultResolver),
		prober:      prober,
	}
}

// watch probes the tunnel at `tunnelURL` until it is judged dead — returning
// the reason — or ctx is cancelled (returns ""). startedAt is when cloudflared
// was launched, for the minLifetime guard.
func (h *funnelHealth) watch(ctx context.Context, tunnelURL string, startedAt time.Time) string {
	u, err := url.Parse(tunnelURL)
	if err != nil || u.Hostname() == "" {
		return "" // nothing sensible to watch; the process exit path still works
	}
	host := u.Hostname()
	healthURL := strings.TrimRight(tunnelURL, "/") + "/health"
	consecutive := 0
	lastDetail := ""
	ticker := time.NewTicker(h.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-ticker.C:
		}
		outcome, detail := h.round(ctx, host, healthURL)
		switch outcome {
		case probeHealthy:
			consecutive = 0
		case probeFailed:
			consecutive++
			lastDetail = detail
		case probeIgnored:
			// leave the streak as it is
		}
		if consecutive >= h.failLimit && time.Since(startedAt) >= h.minLifetime {
			return lastDetail
		}
	}
}

// round runs one probe round. DNS first (cheapest, and NXDOMAIN is the final
// state of a reclaimed tunnel); then the end-to-end GET; then, as a tie-break
// only when both said nothing, cloudflared's own edge-connection count.
func (h *funnelHealth) round(ctx context.Context, host, healthURL string) (probeOutcome, string) {
	if o, d := h.lookup(ctx, host); o == probeFailed {
		return o, d
	}
	o, d := h.get(ctx, healthURL)
	if o != probeIgnored {
		return o, d
	}
	if h.prober != nil {
		if n, err := h.prober.Ready(ctx); err == nil && n == 0 {
			return probeFailed, "cloudflared reports 0 edge connections"
		}
	}
	return probeIgnored, ""
}

// dnsHealthProbe classifies a lookup: NXDOMAIN is the one DNS answer that
// means the tunnel is gone; every other error is the resolver's problem.
func dnsHealthProbe(r *net.Resolver) func(context.Context, string) (probeOutcome, string) {
	return func(ctx context.Context, host string) (probeOutcome, string) {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := r.LookupHost(lctx, host); err != nil {
			var dnsErr *net.DNSError
			if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
				return probeFailed, "hostname is NXDOMAIN"
			}
			return probeIgnored, ""
		}
		return probeIgnored, "" // resolving proves nothing by itself (530 phase)
	}
}

// httpHealthProbe classifies GET /health through the tunnel. Only the daemon's
// own JSON (2xx + application/json + a `status` field) is health; CF's error
// pages, timeouts and transport errors are death; 429 is a throttle.
func httpHealthProbe(client *http.Client) func(context.Context, string) (probeOutcome, string) {
	return func(ctx context.Context, healthURL string) (probeOutcome, string) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return probeIgnored, ""
		}
		res, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return probeIgnored, ""
			}
			return probeFailed, "GET /health failed: " + shortErr(err)
		}
		defer res.Body.Close()
		return classifyHealthResponse(res.StatusCode, res.Header.Get("Content-Type"), res.Body)
	}
}

// classifyHealthResponse is the response-shape rule, on its own for the tests.
func classifyHealthResponse(status int, contentType string, body io.Reader) (probeOutcome, string) {
	if status == http.StatusTooManyRequests {
		return probeIgnored, ""
	}
	if status < 200 || status > 299 {
		return probeFailed, "GET /health answered " + http.StatusText(status) + " (" + itoa(status) + ")"
	}
	if !strings.Contains(strings.ToLower(contentType), "application/json") {
		return probeFailed, "GET /health answered non-JSON (" + contentType + ")"
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 4096)).Decode(&payload); err != nil || payload.Status == "" {
		return probeFailed, "GET /health body is not the daemon's"
	}
	return probeHealthy, ""
}

func itoa(n int) string {
	var b [12]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			break
		}
	}
	return string(b[i:])
}

// shortErr trims Go's nested "Get \"https://...\": dial tcp: ..." chains to
// the part a log reader needs.
func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 && i+2 < len(s) {
		return s[i+2:]
	}
	return s
}
