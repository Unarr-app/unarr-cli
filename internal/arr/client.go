package arr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to a single *arr instance (Sonarr, Radarr, or Prowlarr).
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a client for the given *arr instance.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// WithTimeout overrides the per-request timeout. The sync pages through 1000
// history records at a time, which a slow NAS cannot answer in the default 15 s.
func (c *Client) WithTimeout(d time.Duration) *Client {
	c.httpClient.Timeout = d
	return c
}

// SystemStatus returns version and app info. Works with all *arr apps.
func (c *Client) SystemStatus() (*SystemStatus, error) {
	// Try v3 first (Sonarr/Radarr), then v1 (Prowlarr)
	var s SystemStatus
	if err := c.get("/api/v3/system/status", &s); err != nil {
		if err2 := c.get("/api/v1/system/status", &s); err2 != nil {
			return nil, fmt.Errorf("system/status v3: %w; v1: %v", err, err2)
		}
	}
	return &s, nil
}

// ── Radarr ──────────────────────────────────────────────────────────

func (c *Client) Movies() ([]Movie, error) {
	var m []Movie
	if err := c.get("/api/v3/movie", &m); err != nil {
		return nil, fmt.Errorf("movies: %w", err)
	}
	return m, nil
}

// ── Sonarr ──────────────────────────────────────────────────────────

func (c *Client) Series() ([]Series, error) {
	var s []Series
	if err := c.get("/api/v3/series", &s); err != nil {
		return nil, fmt.Errorf("series: %w", err)
	}
	return s, nil
}

// ── Shared (Sonarr + Radarr use the same v3 endpoints) ─────────────

func (c *Client) QualityProfiles() ([]QualityProfile, error) {
	var p []QualityProfile
	if err := c.get("/api/v3/qualityprofile", &p); err != nil {
		return nil, fmt.Errorf("quality profiles: %w", err)
	}
	return p, nil
}

func (c *Client) RootFolders() ([]RootFolder, error) {
	var f []RootFolder
	if err := c.get("/api/v3/rootfolder", &f); err != nil {
		return nil, fmt.Errorf("root folders: %w", err)
	}
	return f, nil
}

func (c *Client) DownloadClients() ([]DownloadClient, error) {
	var d []DownloadClient
	if err := c.get("/api/v3/downloadclient", &d); err != nil {
		return nil, fmt.Errorf("download clients: %w", err)
	}
	return d, nil
}

// DownloadClientDetails returns the full config (including fields) for a single download client.
func (c *Client) DownloadClientDetails(id int) ([]Field, error) {
	path := fmt.Sprintf("/api/v3/downloadclient/%d", id)
	var dc struct {
		Fields []Field `json:"fields"`
	}
	if err := c.get(path, &dc); err != nil {
		return nil, err
	}
	return dc.Fields, nil
}

// ── Shared (Sonarr + Radarr) ────────────────────────────────────────

func (c *Client) Tags() ([]Tag, error) {
	var t []Tag
	if err := c.get("/api/v3/tag", &t); err != nil {
		return nil, fmt.Errorf("tags: %w", err)
	}
	return t, nil
}

// History returns download history records (grabbed + imported).
// pageSize controls how many records per page (max 250).
func (c *Client) History(pageSize int) ([]HistoryRecord, error) {
	if pageSize <= 0 {
		pageSize = 250
	}
	path := fmt.Sprintf("/api/v3/history?page=1&pageSize=%d&sortKey=date&sortDirection=descending", pageSize)
	var resp HistoryResponse
	if err := c.get(path, &resp); err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	return resp.Records, nil
}

// Blocklist returns releases the user has explicitly rejected.
func (c *Client) Blocklist(pageSize int) ([]BlocklistItem, error) {
	if pageSize <= 0 {
		pageSize = 250
	}
	path := fmt.Sprintf("/api/v3/blocklist?page=1&pageSize=%d", pageSize)
	var resp BlocklistResponse
	if err := c.get(path, &resp); err != nil {
		return nil, fmt.Errorf("blocklist: %w", err)
	}
	return resp.Records, nil
}

// ── Prowlarr ────────────────────────────────────────────────────────

func (c *Client) Indexers() ([]Indexer, error) {
	var idx []Indexer
	if err := c.get("/api/v1/indexer", &idx); err != nil {
		return nil, fmt.Errorf("indexers: %w", err)
	}
	return idx, nil
}

func (c *Client) Applications() ([]Application, error) {
	var apps []Application
	if err := c.get("/api/v1/applications", &apps); err != nil {
		return nil, fmt.Errorf("applications: %w", err)
	}
	return apps, nil
}

// ── HTTP helper ─────────────────────────────────────────────────────

// userAgent identifies the CLI to the *arr instance (and to any reverse proxy
// in front of it); Go's default "Go-http-client/1.1" is what bot filters block.
var userAgent = "unarr-cli"

// SetUserAgent lets cmd/ stamp the version in without an import cycle.
func SetUserAgent(ua string) { userAgent = ua }

// httpStatusError is a non-2xx answer from *arr. Callers match on Status to
// tell "this id does not exist" (404) from a real failure.
type httpStatusError struct {
	Status int
	Body   string
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

func (c *Client) get(path string, dst any) error {
	return c.do(http.MethodGet, path, nil, dst)
}

// do sends one request; body (when non-nil) is JSON-encoded and dst (when
// non-nil) receives the decoded response.
func (c *Client) do(method, path string, body, dst any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20)) // 50MB limit for large libraries
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("unauthorized — check your API key")
	}
	if resp.StatusCode >= 400 {
		msg := string(respBody)
		if len(msg) > 200 {
			msg = msg[:200] + "..."
		}
		return &httpStatusError{Status: resp.StatusCode, Body: msg}
	}
	if dst == nil {
		return nil
	}

	if err := json.Unmarshal(respBody, dst); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}
