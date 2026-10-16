package agent

import (
	"context"
	"encoding/json"
)

// sessionsPublishRequest is the POST /api/internal/agent/sessions body. The
// snapshot is the daemon's GET /sessions document (schemaVersion 1), passed
// through as already encoded.
type sessionsPublishRequest struct {
	AgentID  string          `json:"agentId"`
	Snapshot json.RawMessage `json:"snapshot"`
}

// PublishSessions sends one session snapshot to the web. Errors from the server
// are *HTTPError (an old web answers 404); ctx carries the caller's timeout.
func (c *Client) PublishSessions(ctx context.Context, agentID string, snapshot []byte) error {
	return c.doPost(ctx, "/api/internal/agent/sessions", sessionsPublishRequest{AgentID: agentID, Snapshot: snapshot}, nil)
}
