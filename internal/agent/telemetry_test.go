package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newCountingServer returns a test server that counts POSTs to /agent/event and
// captures the last decoded payload.
func newCountingServer(t *testing.T) (*httptest.Server, *int32, *reportEventRequest) {
	t.Helper()
	var hits int32
	var last reportEventRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/internal/agent/event" {
			atomic.AddInt32(&hits, 1)
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &last)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &last
}

func TestEmitter_DisabledIsNoOp(t *testing.T) {
	srv, hits, _ := newCountingServer(t)
	c := NewClient(srv.URL, "test-key", "unarr-test")

	// enabled=false → nothing must reach the network, neither async nor sync.
	e := NewEmitter(c, false, "agent-123", "1.8.0", "linux")
	e.Emit(EventExitCrash, "boom")
	e.EmitSync(EventConfigError, "no dir")

	// Give any (erroneously spawned) async goroutine a chance to fire.
	time.Sleep(50 * time.Millisecond)

	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("disabled emitter sent %d requests, want 0", got)
	}
}

func TestEmitter_NilIsNoOp(t *testing.T) {
	var e *Emitter // nil
	// Must not panic.
	e.Emit(EventLoginOK, "")
	e.EmitSync(EventLoginOK, "")
	if e.Enabled() {
		t.Fatal("nil emitter reported Enabled() = true")
	}
}

func TestEmitter_EmitSyncSendsPayload(t *testing.T) {
	srv, hits, last := newCountingServer(t)
	c := NewClient(srv.URL, "test-key", "unarr-test")

	e := NewEmitter(c, true, "agent-123", "1.8.0", "darwin")
	e.EmitSync(EventPortInUse, "11818")

	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("enabled EmitSync sent %d requests, want 1", got)
	}
	if last.Event.Type != EventPortInUse {
		t.Errorf("type = %q, want %q", last.Event.Type, EventPortInUse)
	}
	if last.Event.Detail != "11818" {
		t.Errorf("detail = %q, want %q", last.Event.Detail, "11818")
	}
	if last.Event.AgentID != "agent-123" {
		t.Errorf("agentId = %q, want agent-123", last.Event.AgentID)
	}
	if last.Event.Version != "1.8.0" || last.Event.OS != "darwin" {
		t.Errorf("version/os = %q/%q, want 1.8.0/darwin", last.Event.Version, last.Event.OS)
	}
}

func TestEmitter_EmitAsyncEventuallySends(t *testing.T) {
	srv, hits, _ := newCountingServer(t)
	c := NewClient(srv.URL, "test-key", "unarr-test")

	e := NewEmitter(c, true, "agent-123", "1.8.0", "windows")
	e.Emit(EventExitUserQuit, "interrupt")

	// Async — poll briefly for the goroutine's post to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(hits) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("async Emit never sent (hits=%d)", atomic.LoadInt32(hits))
}
