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
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	// wakeClient has no built-in timeout — used exclusively for the long-poll
	// wake endpoint where the context controls cancellation.
	wakeClient *http.Client
	userAgent  string
}

// NewClient creates an agent API client.
func NewClient(baseURL, apiKey, userAgent string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		// wakeClient has no built-in timeout — the context controls it.
		// The server holds the connection for up to 28s before responding.
		wakeClient: &http.Client{},
		userAgent:  userAgent,
	}
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
	url := fmt.Sprintf("/api/internal/agent/nzb-download?nzbId=%s", nzbID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("nzb download error %d: %s", resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20)) // 100MB limit
	if err != nil {
		return nil, fmt.Errorf("read nzb: %w", err)
	}
	return data, nil
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
func (c *Client) SyncLibrary(ctx context.Context, req LibrarySyncRequest) (*LibrarySyncResponse, error) {
	var resp LibrarySyncResponse
	if err := c.doPost(ctx, "/api/internal/agent/library-sync", req, &resp); err != nil {
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
func (c *Client) WaitForWake(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/internal/agent/wake", nil)
	if err != nil {
		return false, fmt.Errorf("create wake request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.wakeClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("wake request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return false, &HTTPError{StatusCode: resp.StatusCode, Message: string(body)}
	}

	var result struct {
		Wake bool `json:"wake"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Errorf("decode wake response: %w", err)
	}
	return result.Wake, nil
}

// doPost sends a JSON POST request and decodes the response.
func (c *Client) doPost(ctx context.Context, path string, body any, dst any) error {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	return c.handleResponse(resp, dst)
}

// doGet sends a GET request and decodes the response.
func (c *Client) doGet(ctx context.Context, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
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
