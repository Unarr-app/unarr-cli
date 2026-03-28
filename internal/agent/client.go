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
		userAgent: userAgent,
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

// Heartbeat sends a periodic keep-alive signal.
func (c *Client) Heartbeat(ctx context.Context, req HeartbeatRequest) error {
	var resp StatusResponse
	if err := c.doPost(ctx, "/api/internal/agent/heartbeat", req, &resp); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	return nil
}

// ClaimTasks polls for pending download tasks and claims them atomically.
func (c *Client) ClaimTasks(ctx context.Context, agentID string) ([]Task, error) {
	url := fmt.Sprintf("/api/internal/agent/tasks?agentId=%s", agentID)
	var resp TasksResponse
	if err := c.doGet(ctx, url, &resp); err != nil {
		return nil, fmt.Errorf("claim tasks: %w", err)
	}
	return resp.Tasks, nil
}

// ReportStatus reports download progress or completion for a task.
// ReportStatus reports download progress. Returns server-side flags the CLI must act on.
func (c *Client) ReportStatus(ctx context.Context, update StatusUpdate) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.doPost(ctx, "/api/internal/agent/status", update, &resp); err != nil {
		return nil, fmt.Errorf("report status: %w", err)
	}
	return &resp, nil
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
			return fmt.Errorf("API error %d: %s", resp.StatusCode, errResp.Error)
		}
		// Non-JSON response (e.g. HTML error page) — truncate to something readable
		msg := string(body)
		if len(msg) > 120 || strings.Contains(msg, "<html") || strings.Contains(msg, "<!DOCTYPE") {
			msg = fmt.Sprintf("server returned %s (non-JSON response, likely a server error)", resp.Status)
		}
		return fmt.Errorf("API error %d: %s", resp.StatusCode, msg)
	}

	if dst != nil {
		if err := json.Unmarshal(body, dst); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}

	return nil
}
