package arr

import (
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

func (c *Client) get(path string, dst any) error {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20)) // 50MB limit for large libraries
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("unauthorized — check your API key")
	}
	if resp.StatusCode >= 400 {
		msg := string(body)
		if len(msg) > 200 {
			msg = msg[:200] + "..."
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}
