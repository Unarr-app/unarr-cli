package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrNoDaemon is returned when there is no control plane to talk to — the
// daemon is stopped, or it is an old build without one. Callers fall back to
// the offline path (editing the resume store directly), which is exactly what a
// user with a runaway download needs.
var ErrNoDaemon = errors.New("no running daemon with a control plane")

// Client talks to a daemon's local control plane.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient builds a client for a control plane on the given loopback port.
func NewClient(port int, token string) *Client {
	return &Client{
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		token:   token,
		// Generous but bounded: a cancel has to drop a torrent handle and can
		// briefly block on disk, and a user hammering Ctrl+C at a hung daemon is
		// worse served by a fast failure than by waiting two seconds.
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// List returns every download the daemon knows about.
func (c *Client) List(ctx context.Context) ([]TaskInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/tasks", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(TokenHeader, c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoDaemon, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("control plane returned %d", resp.StatusCode)
	}
	var out ListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode control response: %w", err)
	}
	return out.Tasks, nil
}

// Do runs an action and returns its per-task results.
func (c *Client) Do(ctx context.Context, action string, req ActionRequest) ([]ActionResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/tasks/"+action, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set(TokenHeader, c.token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoDaemon, err)
	}
	defer resp.Body.Close()

	var out ActionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && resp.StatusCode == http.StatusOK {
		return nil, fmt.Errorf("decode control response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if out.Error != "" {
			return nil, errors.New(out.Error)
		}
		return nil, fmt.Errorf("control plane returned %d", resp.StatusCode)
	}
	return out.Results, nil
}
