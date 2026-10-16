package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The body is the contract the web validates: {"agentId", "snapshot"} with the
// snapshot passed through byte for byte, on the agent's usual auth.
func TestPublishSessionsBody(t *testing.T) {
	var got struct {
		AgentID  string          `json:"agentId"`
		Snapshot json.RawMessage `json:"snapshot"`
	}
	var auth, method, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/sessions" {
			http.NotFound(w, r)
			return
		}
		auth, method, ctype = r.Header.Get("Authorization"), r.Method, r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	snap := `{"schemaVersion":1,"sessions":[]}`
	c := NewClient(srv.URL, "k3y", "unarr-test")
	if err := c.PublishSessions(context.Background(), "agent-1", []byte(snap)); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || auth != "Bearer k3y" || ctype != "application/json" {
		t.Fatalf("method=%s auth=%q content-type=%q", method, auth, ctype)
	}
	if got.AgentID != "agent-1" || string(got.Snapshot) != snap {
		t.Fatalf("agentId=%q snapshot=%s, want agent-1 and %s", got.AgentID, got.Snapshot, snap)
	}
}

// Errors surface as *HTTPError so the caller can tell an old web (404) from a
// refused body (413).
func TestPublishSessionsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":"payload_too_large"}`))
	}))
	t.Cleanup(srv.Close)

	err := NewClient(srv.URL, "k", "ua").PublishSessions(context.Background(), "a", []byte(`{}`))
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("err = %v, want an HTTPError 413", err)
	}
}

// A snapshot that is not JSON is a caller bug, refused before the network.
func TestPublishSessionsRejectsInvalidJSON(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	t.Cleanup(srv.Close)
	if err := NewClient(srv.URL, "k", "ua").PublishSessions(context.Background(), "a", []byte(`{`)); err == nil || hit {
		t.Fatalf("err=%v hit=%v, want a local error and no request", err, hit)
	}
}
