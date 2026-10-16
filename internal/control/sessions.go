package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/metrics"
)

// handleSessions serves GET /sessions: the unified session view
// (metrics.Snapshot, schemaVersion 1). Read-only; 404 when the daemon did not
// wire a source, so an old client can tell "not supported" from "no sessions".
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.sessions == nil {
		http.Error(w, "sessions not available", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, s.sessions.Poll(time.Now()))
}

// Sessions fetches the daemon's unified session snapshot.
func (c *Client) Sessions(ctx context.Context) (metrics.Snapshot, error) {
	var out metrics.Snapshot
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/sessions", nil)
	if err != nil {
		return out, err
	}
	req.Header.Set(TokenHeader, c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return out, fmt.Errorf("%w: %v", ErrNoDaemon, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return out, fmt.Errorf("%w: the running daemon predates /sessions (update it with `unarr update`)", ErrNoDaemon)
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("control plane returned %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("decode control response: %w", err)
	}
	return out, nil
}
