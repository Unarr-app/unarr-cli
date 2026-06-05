package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client communicates with the /api/internal/agent/* endpoints.
//
// The client owns a MirrorPool: when a request fails with a transient
// network error (DNS, refused, timeout, 5xx) it rotates to the next mirror
// and retries up to `len(mirrors)-1` times so a single agent run survives
// a primary-domain takedown without user intervention.
type Client struct {
	pool       *MirrorPool
	apiKey     string
	httpClient *http.Client
	// wakeClient has no built-in timeout — used exclusively for the long-poll
	// wake endpoint where the context controls cancellation.
	wakeClient *http.Client
	// librarySyncClient has a generous timeout for library-sync calls which can
	// take several minutes when syncing hundreds or thousands of items.
	librarySyncClient *http.Client
	userAgent         string
}

// NewClient creates an agent API client targeting a single base URL.
// Equivalent to NewClientWithMirrors(baseURL, nil, ...) — kept for callers
// that don't yet care about mirror failover.
func NewClient(baseURL, apiKey, userAgent string) *Client {
	return NewClientWithMirrors(baseURL, nil, apiKey, userAgent)
}

// NewClientWithMirrors creates an agent API client that can fail over from
// the primary base URL to any of the extras when the primary is unreachable.
// The order of `extras` matters: they're tried left-to-right after a failure.
func NewClientWithMirrors(baseURL string, extras []string, apiKey, userAgent string) *Client {
	return &Client{
		pool:   NewMirrorPool(baseURL, extras),
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		// wakeClient has no built-in timeout — the context controls it.
		// The server holds the connection for up to 28s before responding.
		wakeClient: &http.Client{},
		// librarySyncClient uses a 10-minute timeout to handle large libraries
		// (hundreds or thousands of items) where ffprobe scanning alone can take
		// several minutes before the HTTP request is even sent.
		librarySyncClient: &http.Client{Timeout: 10 * time.Minute},
		userAgent:         userAgent,
	}
}

// MirrorPool exposes the underlying pool so callers (e.g. the `unarr mirrors`
// subcommand) can swap the list at runtime after fetching /api/v1/mirrors.
func (c *Client) MirrorPool() *MirrorPool {
	return c.pool
}

// baseURL returns the currently-active mirror. Routed through this helper so
// future changes (e.g. per-endpoint mirror affinity) only need one edit.
func (c *Client) baseURL() string {
	return c.pool.Current()
}

// Register registers the CLI agent with the server and returns user info + features.
func (c *Client) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	var resp RegisterResponse
	if err := c.doPost(ctx, "/api/internal/agent/register", req, &resp); err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	return &resp, nil
}

// Deregister notifies the server that the agent is shutting down.
func (c *Client) Deregister(ctx context.Context, agentID string) error {
	req := struct {
		AgentID string `json:"agentId"`
	}{AgentID: agentID}
	var resp StatusResponse
	if err := c.doPost(ctx, "/api/internal/agent/deregister", req, &resp); err != nil {
		return fmt.Errorf("deregister: %w", err)
	}
	return nil
}

// ReportUpgradeResult tells the server the outcome of a previously requested
// upgrade so the server can clear `upgrade_requested`. Without this call the
// flag stays sticky and the daemon would re-trigger applyAutoUpgrade on every
// sync after upgrade — even for "already on target version" no-ops.
func (c *Client) ReportUpgradeResult(ctx context.Context, agentID string, success bool, version, errMsg string) error {
	req := struct {
		AgentID string `json:"agentId"`
		Success bool   `json:"success"`
		Version string `json:"version,omitempty"`
		Error   string `json:"error,omitempty"`
	}{AgentID: agentID, Success: success, Version: version, Error: errMsg}
	var resp StatusResponse
	if err := c.doPost(ctx, "/api/internal/agent/upgrade-result", req, &resp); err != nil {
		return fmt.Errorf("report upgrade result: %w", err)
	}
	return nil
}

// MarkSessionReady signals the server that the first HLS segment + init.mp4
// landed on disk for the given session. The web side flips
// streaming_session.ready_at = NOW(), which its SSE endpoint emits to
// subscribed players so the "Preparando…" UI ends without polling HEAD
// on /hls/<id>/master.m3u8.
//
// Best-effort: the server is the source of truth for session state and
// will reach the same conclusion via HEAD probes anyway if this call
// fails. We log the error in the caller but don't retry — by the time
// a retry would land the user is likely already playing.
func (c *Client) MarkSessionReady(ctx context.Context, sessionID string, health *SessionHealth) error {
	req := struct {
		SessionID string         `json:"sessionId"`
		Health    *SessionHealth `json:"health,omitempty"`
	}{SessionID: sessionID, Health: health}
	var resp StatusResponse
	if err := c.doPost(ctx, "/api/internal/agent/session-ready", req, &resp); err != nil {
		return fmt.Errorf("mark session ready: %w", err)
	}
	return nil
}

// SessionHealth is an OPTIONAL live-transcode health snapshot attached to a
// session-ready report (F3). A nil *SessionHealth means the agent has no
// telemetry to share (cache hit, direct-play, or progress not yet stable) and
// the web side keeps its stall-shape heuristic. Old web replicas ignore the
// extra field; old agents simply never send it.
type SessionHealth struct {
	// "ok" (≥ realtime) | "marginal" (keeps up barely) | "struggling" (can't).
	Health string `json:"health"`
	// ffmpeg speed= EWMA: 1.0 = exactly realtime, < 1.0 = slower than playback.
	RealtimeRatio float64 `json:"realtimeRatio"`
	// "realtime" | "transcode" (encoder is the wall) | "input_bound" (source read).
	Reason string `json:"reason"`
}

// RefreshStreamURL re-resolves a fresh debrid direct URL for a live streaming
// session (hueco #2 / 2c). Called by the daemon when a debrid source expires
// mid-stream (the link is time-limited; the content is still cached). Returns
// the new URL on success; an error (incl. 409/410) means refresh isn't
// possible and the caller should stop trying.
func (c *Client) RefreshStreamURL(ctx context.Context, sessionID string) (string, error) {
	req := struct {
		SessionID string `json:"sessionId"`
	}{SessionID: sessionID}
	var resp struct {
		DirectURL string `json:"directUrl"`
	}
	if err := c.doPost(ctx, "/api/internal/agent/stream-url", req, &resp); err != nil {
		return "", fmt.Errorf("refresh stream url: %w", err)
	}
	if resp.DirectURL == "" {
		return "", fmt.Errorf("refresh stream url: empty url in response")
	}
	return resp.DirectURL, nil
}

// ReportStatus reports download progress. Returns server-side flags the CLI must act on.
func (c *Client) ReportStatus(ctx context.Context, update StatusUpdate) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.doPost(ctx, "/api/internal/agent/status", update, &resp); err != nil {
		return nil, fmt.Errorf("report status: %w", err)
	}
	return &resp, nil
}

// BatchReportStatus sends multiple status updates in a single request.
func (c *Client) BatchReportStatus(ctx context.Context, updates []StatusUpdate) (*BatchStatusResponse, error) {
	var resp BatchStatusResponse
	if err := c.doPost(ctx, "/api/internal/agent/status", BatchStatusRequest{Updates: updates}, &resp); err != nil {
		return nil, fmt.Errorf("batch report status: %w", err)
	}
	return &resp, nil
}

// Sync sends the CLI's full state and receives all pending server actions.
// This is the single endpoint for bidirectional state synchronization.
func (c *Client) Sync(ctx context.Context, req SyncRequest) (*SyncResponse, error) {
	var resp SyncResponse
	if err := c.doPost(ctx, "/api/internal/agent/sync", req, &resp); err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Usenet endpoints
// ---------------------------------------------------------------------------

// SearchNzbs searches NZB indexers for matching content.
func (c *Client) SearchNzbs(ctx context.Context, params NzbSearchParams) (*NzbSearchResponse, error) {
	var resp NzbSearchResponse
	if err := c.doPost(ctx, "/api/internal/agent/nzb-search", params, &resp); err != nil {
		return nil, fmt.Errorf("nzb search: %w", err)
	}
	return &resp, nil
}

// DownloadNzb downloads the NZB file for the given nzbId.
// Returns the raw NZB XML bytes.
func (c *Client) DownloadNzb(ctx context.Context, nzbID string) ([]byte, error) {
	path := fmt.Sprintf("/api/internal/agent/nzb-download?nzbId=%s", nzbID)

	var out []byte
	err := c.withMirrorFailover(func(base string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		c.setHeaders(req)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			return &HTTPError{StatusCode: resp.StatusCode, Message: string(body)}
		}

		data, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20)) // 100MB limit
		if err != nil {
			return fmt.Errorf("read nzb: %w", err)
		}
		out = data
		return nil
	})
	return out, err
}

// GetUsenetCredentials fetches NNTP connection credentials.
func (c *Client) GetUsenetCredentials(ctx context.Context) (*UsenetCredentials, error) {
	var resp UsenetCredentials
	if err := c.doGet(ctx, "/api/internal/agent/usenet-credentials", &resp); err != nil {
		return nil, fmt.Errorf("usenet credentials: %w", err)
	}
	return &resp, nil
}

// GetUsenetUsage fetches current month's usenet quota usage.
func (c *Client) GetUsenetUsage(ctx context.Context) (*UsenetUsageResponse, error) {
	var resp UsenetUsageResponse
	if err := c.doGet(ctx, "/api/internal/agent/usenet-usage", &resp); err != nil {
		return nil, fmt.Errorf("usenet usage: %w", err)
	}
	return &resp, nil
}

// ConfigureDebrid saves a debrid provider token for the user (used by unarr init/migrate).
func (c *Client) ConfigureDebrid(ctx context.Context, req ConfigureDebridRequest) (*ConfigureDebridResponse, error) {
	var resp ConfigureDebridResponse
	if err := c.doPost(ctx, "/api/internal/agent/debrid-config", req, &resp); err != nil {
		return nil, fmt.Errorf("configure debrid: %w", err)
	}
	return &resp, nil
}

// BatchDownload queues multiple items for download (used by unarr migrate).
func (c *Client) BatchDownload(ctx context.Context, req BatchDownloadRequest) (*BatchDownloadResponse, error) {
	var resp BatchDownloadResponse
	if err := c.doPost(ctx, "/api/internal/agent/batch-download", req, &resp); err != nil {
		return nil, fmt.Errorf("batch download: %w", err)
	}
	return &resp, nil
}

// SyncLibrary sends scanned library items to the server for matching and upgrade discovery.
// Uses a 10-minute timeout client to handle large libraries where scanning can take several minutes.
func (c *Client) SyncLibrary(ctx context.Context, req LibrarySyncRequest) (*LibrarySyncResponse, error) {
	var resp LibrarySyncResponse
	if err := c.doPostWith(ctx, c.librarySyncClient, "/api/internal/agent/library-sync", req, &resp); err != nil {
		return nil, fmt.Errorf("library sync: %w", err)
	}
	return &resp, nil
}

// ReportWatchProgress sends playback position to the server for watch tracking.
func (c *Client) ReportWatchProgress(ctx context.Context, update WatchProgressUpdate) error {
	var resp WatchProgressResponse
	if err := c.doPost(ctx, "/api/internal/agent/watch-progress", update, &resp); err != nil {
		return fmt.Errorf("watch progress: %w", err)
	}
	return nil
}

// WaitForWake blocks until the server sends a wake signal, the long-poll
// timeout elapses, or ctx is cancelled. Returns true when a wake signal
// was received (caller should sync immediately), false on timeout/cancel.
//
// Wake is a long-poll on a single mirror — failover here would just drop
// the connection and try again immediately, which the server already
// handles with a fresh wait loop. We only retry against the next mirror
// when the current one is definitively unreachable (DNS / refused / TLS).
func (c *Client) WaitForWake(ctx context.Context) (bool, error) {
	var wake bool
	err := c.withMirrorFailover(func(base string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/internal/agent/wake", nil)
		if err != nil {
			return fmt.Errorf("create wake request: %w", err)
		}
		c.setHeaders(req)

		resp, err := c.wakeClient.Do(req)
		if err != nil {
			return fmt.Errorf("wake request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
			return &HTTPError{StatusCode: resp.StatusCode, Message: string(body)}
		}

		var result struct {
			Wake bool `json:"wake"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return fmt.Errorf("decode wake response: %w", err)
		}
		wake = result.Wake
		return nil
	})
	return wake, err
}

// doPost sends a JSON POST request using the default httpClient and decodes the response.
func (c *Client) doPost(ctx context.Context, path string, body any, dst any) error {
	return c.doPostWith(ctx, c.httpClient, path, body, dst)
}

// doPostWith sends a JSON POST request using the provided HTTP client and decodes the response.
// Use this to override the default timeout for specific operations (e.g. librarySyncClient).
// Wrapped in withMirrorFailover so a transient connection failure on the
// active mirror retries against the next one.
func (c *Client) doPostWith(ctx context.Context, hc *http.Client, path string, body any, dst any) error {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	return c.withMirrorFailover(func(base string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(jsonBody))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}

		c.setHeaders(req)
		req.Header.Set("Content-Type", "application/json")

		resp, err := hc.Do(req)
		if err != nil {
			return fmt.Errorf("request failed: %w", err)
		}
		defer resp.Body.Close()

		return c.handleResponse(resp, dst)
	})
}

// doGet sends a GET request and decodes the response.
func (c *Client) doGet(ctx context.Context, path string, dst any) error {
	return c.withMirrorFailover(func(base string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}

		c.setHeaders(req)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("request failed: %w", err)
		}
		defer resp.Body.Close()

		return c.handleResponse(resp, dst)
	})
}

// withMirrorFailover runs `fn` against the current mirror; on a transient
// error it rotates the pool and retries up to `len(mirrors)-1` times.
//
// The active mirror is updated on rotation so subsequent unrelated calls
// stick to the working host until that host fails too — this avoids
// hammering a known-bad primary on every request, while still trying it
// again next time the agent reloads (no permanent demotion).
func (c *Client) withMirrorFailover(fn func(base string) error) error {
	attempts := c.pool.Len()
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		base := c.baseURL()
		err := fn(base)
		if err == nil {
			return nil
		}
		lastErr = err
		if !IsTransient(err) {
			return err
		}
		// Last attempt: don't bother rotating, just surface the error.
		if i == attempts-1 {
			break
		}
		next, rotated := c.pool.Rotate()
		if !rotated {
			break
		}
		_ = next // mirror rotation logging is left to higher layers (cmd/) so the
		// pool stays log-free for tests.
	}
	return lastErr
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
}

func (c *Client) handleResponse(resp *http.Response, dst any) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode >= 400 {
		// Try to parse as JSON error
		var errResp ErrorResponse
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			return &HTTPError{StatusCode: resp.StatusCode, Message: errResp.Error}
		}
		// Non-JSON response (e.g. HTML error page) — truncate to something readable
		msg := string(body)
		if len(msg) > 120 || strings.Contains(msg, "<html") || strings.Contains(msg, "<!DOCTYPE") {
			msg = fmt.Sprintf("server returned %s (non-JSON response, likely a server error)", resp.Status)
		}
		return &HTTPError{StatusCode: resp.StatusCode, Message: msg}
	}

	if dst != nil {
		if err := json.Unmarshal(body, dst); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}

	return nil
}
