package agent

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// MirrorRoundTripper gives any *http.Client the same mirror failover the agent
// control-plane Client has: on a transient transport error or a retryable 5xx
// it rewrites the request to the next mirror in the shared MirrorPool and
// retries. It exists so the public-API go-client stops diverging from the agent
// client — both now survive a primary-domain takedown using the SAME pool and
// the SAME transient-error policy (IsTransient).
//
// Requests whose body cannot be replayed (Body != nil && GetBody == nil) are
// sent once with no failover, so a consumed body is never re-read. Standard
// library requests built with a *bytes.Reader/strings.Reader (and all GETs) set
// GetBody, so this only affects exotic streaming bodies the public API doesn't use.
type MirrorRoundTripper struct {
	pool  *MirrorPool
	inner http.RoundTripper
}

// NewMirrorRoundTripper wraps inner (defaults to http.DefaultTransport) with
// failover across pool's mirrors.
func NewMirrorRoundTripper(pool *MirrorPool, inner http.RoundTripper) *MirrorRoundTripper {
	if inner == nil {
		inner = http.DefaultTransport
	}
	return &MirrorRoundTripper{pool: pool, inner: inner}
}

// RoundTrip points the request at the current mirror and, on a transient
// failure, rotates the pool and retries against the next one. A non-transient
// HTTP status (4xx, or a 5xx IsTransient doesn't retry) or a non-replayable body
// is returned to the caller unchanged.
func (m *MirrorRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	attempts := 1
	if req.Body == nil || req.GetBody != nil { // replayable → may fail over
		if n := m.pool.Len(); n > attempts {
			attempts = n
		}
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		out := req.Clone(req.Context())
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("mirror transport: rebuild body: %w", err)
			}
			out.Body = body
		}
		if base, err := url.Parse(m.pool.Current()); err == nil && base.Host != "" {
			out.URL.Scheme = base.Scheme
			out.URL.Host = base.Host
			out.Host = base.Host
		}

		resp, err := m.inner.RoundTrip(out)
		last := i == attempts-1
		switch {
		case err != nil:
			if last || !IsTransient(err) {
				return nil, err
			}
			lastErr = err
		case resp.StatusCode >= 400 && IsTransient(classifyStatus(resp)):
			if last {
				return resp, nil // surface the real 5xx to the caller
			}
			resp.Body.Close()
			lastErr = fmt.Errorf("mirror %s: HTTP %d", out.URL.Host, resp.StatusCode)
		default:
			return resp, nil // success, or a status we must not retry (4xx/auth)
		}

		if _, rotated := m.pool.Rotate(); !rotated {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("mirror transport: all mirrors failed")
	}
	return nil, lastErr
}

// classifyStatus turns a failed response into the HTTPError IsTransient judges.
//
// The status alone is not enough for a 404: only the edge's own not-found (a
// router with no backend, mid-deploy) is worth another mirror, and telling it
// from the API's 404 takes the body. Read here and put back, so a caller that
// gets this very response still reads it whole; every other status is judged on
// the code alone and the body is left untouched.
func classifyStatus(resp *http.Response) *HTTPError {
	if resp.StatusCode != http.StatusNotFound || resp.Body == nil {
		return &HTTPError{StatusCode: resp.StatusCode}
	}
	// The edge's body is 18 bytes; anything longer is someone else's 404.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return &HTTPError{StatusCode: resp.StatusCode}
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}
	return httpErrorFromBody(resp.StatusCode, body)
}
