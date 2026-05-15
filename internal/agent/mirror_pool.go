package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// MirrorPool holds the ordered list of API base URLs the client is willing to
// fall back to when the current mirror is unreachable. The first entry is
// always the "preferred" mirror configured by the user. Subsequent entries
// are alternate domains we can rotate to without changing any user-visible
// configuration — they exist so a long-lived agent survives a takedown of
// the primary host without needing a new release.
//
// The pool is concurrency-safe; rotation is a fast O(1) index bump under a
// mutex. The previously-active mirror is NEVER removed — it might just be
// temporarily unreachable from one network path.
type MirrorPool struct {
	mu      sync.RWMutex
	mirrors []string
	current int
}

// NewMirrorPool builds a pool from the provided base URLs. The primary URL
// is always first; "extras" are appended in order and de-duplicated. Empty
// strings are skipped. Trailing slashes are normalised so callers can concat
// `pool.Current() + "/api/..."` reliably.
func NewMirrorPool(primary string, extras []string) *MirrorPool {
	seen := make(map[string]struct{})
	var out []string

	add := func(raw string) {
		raw = strings.TrimRight(strings.TrimSpace(raw), "/")
		if raw == "" {
			return
		}
		if _, dup := seen[raw]; dup {
			return
		}
		seen[raw] = struct{}{}
		out = append(out, raw)
	}

	add(primary)
	for _, e := range extras {
		add(e)
	}

	if len(out) == 0 {
		// Defensive: always return a pool with at least one entry so callers
		// can call Current() without nil checks. The empty string would
		// produce obvious errors immediately, which is preferable to a panic
		// somewhere deep in net/http.
		out = []string{""}
	}

	return &MirrorPool{mirrors: out}
}

// Current returns the active base URL.
func (p *MirrorPool) Current() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mirrors[p.current]
}

// Mirrors returns a copy of the configured base URLs in priority order.
func (p *MirrorPool) Mirrors() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, len(p.mirrors))
	copy(out, p.mirrors)
	return out
}

// Len reports how many mirrors are configured.
func (p *MirrorPool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.mirrors)
}

// Rotate moves the cursor to the next mirror in the pool, wrapping around.
// Returns the new current mirror and whether a rotation actually happened
// (a single-mirror pool returns false).
func (p *MirrorPool) Rotate() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.mirrors) <= 1 {
		return p.mirrors[p.current], false
	}
	p.current = (p.current + 1) % len(p.mirrors)
	return p.mirrors[p.current], true
}

// Replace swaps the entire mirror set, e.g. after `unarr mirrors update`
// downloaded a fresh list from /api/v1/mirrors. Resets the cursor to 0 so
// the newly-discovered primary is tried first.
func (p *MirrorPool) Replace(primary string, extras []string) {
	fresh := NewMirrorPool(primary, extras)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mirrors = fresh.mirrors
	p.current = 0
}

// IsTransient reports whether an error is the kind we should retry against
// another mirror. The intent is conservative: rotate on connection-level
// failures (DNS, refused, TLS, timeouts, 5xx) but NOT on auth or validation
// errors that would just fail again somewhere else.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}

	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
			http.StatusRequestTimeout:
			return true
		}
		// 4xx (auth, rate limit, validation) won't get healthier on another mirror.
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// `connection refused`, `EOF`, `tls: ...` end up as wrapped url.Errors.
		msg := urlErr.Error()
		if strings.Contains(msg, "connection refused") ||
			strings.Contains(msg, "no such host") ||
			strings.Contains(msg, "EOF") ||
			strings.Contains(msg, "tls:") ||
			strings.Contains(msg, "i/o timeout") ||
			strings.Contains(msg, "network is unreachable") {
			return true
		}
	}

	// Bare strings as last resort — net.OpError messages are unstable across Go versions.
	msg := err.Error()
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "network is unreachable") {
		return true
	}

	return false
}
