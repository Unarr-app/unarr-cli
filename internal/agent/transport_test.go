package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ── HTTP Transport Tests ─────────────────────────────────────────────────────

func TestHTTPTransportMode(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	if tr.Mode() != "http" {
		t.Errorf("expected http, got %s", tr.Mode())
	}
}

func TestHTTPTransportEventsNeverEmit(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	select {
	case <-tr.Events():
		t.Error("events channel should never emit in HTTP mode")
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestHTTPTransportDelegates(t *testing.T) {
	// Mock server for register
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(RegisterResponse{
			Success: true,
			User:    UserInfo{Name: "Test", Plan: "pro"},
		})
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL, "test-key", "test-agent")
	resp, err := tr.Register(context.Background(), RegisterRequest{AgentID: "a1"})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}
	if resp.User.Name != "Test" {
		t.Errorf("expected Test, got %s", resp.User.Name)
	}
}

// ── WebSocket Transport Tests ────────────────────────────────────────────────

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func TestWSTransportConnectAndAuth(t *testing.T) {
	var received wsAuthMessage
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()

		// Read auth message
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		mu.Lock()
		json.Unmarshal(msg, &received)
		mu.Unlock()

		// Send registered response
		conn.WriteJSON(wsRegisteredMessage{
			Type:     "registered",
			User:     UserInfo{Name: "WS User", Plan: "pro", IsPro: true},
			Features: FeatureFlags{Torrent: true},
		})

		// Keep connection open
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	tr := NewWSTransport(wsURL, "my-api-key", "agent-123", "test/1.0")

	ctx := context.Background()
	if err := tr.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer tr.Close()

	resp, err := tr.Register(ctx, RegisterRequest{
		AgentID: "agent-123",
		Name:    "test-agent",
		Version: "1.0.0",
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}
	if resp.User.Name != "WS User" {
		t.Errorf("expected WS User, got %s", resp.User.Name)
	}

	mu.Lock()
	if received.APIKey != "my-api-key" {
		t.Errorf("expected my-api-key, got %s", received.APIKey)
	}
	if received.AgentID != "agent-123" {
		t.Errorf("expected agent-123, got %s", received.AgentID)
	}
	mu.Unlock()
}

func TestWSTransportReceiveTasks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// Read auth
		conn.ReadMessage()
		conn.WriteJSON(wsRegisteredMessage{
			Type: "registered",
			User: UserInfo{Name: "Test"},
		})

		// Push tasks
		time.Sleep(50 * time.Millisecond)
		conn.WriteJSON(wsTasksMessage{
			Type: "tasks",
			Tasks: []Task{
				{ID: "t1", InfoHash: "abc123", Title: "Test Movie"},
				{ID: "t2", InfoHash: "def456", Title: "Test Show"},
			},
		})

		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	tr := NewWSTransport(wsURL, "key", "agent1", "ua")

	ctx := context.Background()
	tr.Connect(ctx)
	defer tr.Close()

	tr.Register(ctx, RegisterRequest{AgentID: "agent1"})

	// Wait for tasks event
	select {
	case event := <-tr.Events():
		if event.Type != "tasks" {
			t.Errorf("expected tasks, got %s", event.Type)
		}
		if len(event.Tasks.Tasks) != 2 {
			t.Errorf("expected 2 tasks, got %d", len(event.Tasks.Tasks))
		}
		if event.Tasks.Tasks[0].Title != "Test Movie" {
			t.Errorf("expected Test Movie, got %s", event.Tasks.Tasks[0].Title)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for tasks event")
	}
}

func TestWSTransportReceiveControl(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		conn.ReadMessage()
		conn.WriteJSON(wsRegisteredMessage{Type: "registered", User: UserInfo{}})

		time.Sleep(50 * time.Millisecond)
		conn.WriteJSON(map[string]string{
			"type":   "control",
			"action": "cancel",
			"taskId": "task-99",
		})

		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	tr := NewWSTransport(wsURL, "key", "a1", "ua")

	ctx := context.Background()
	tr.Connect(ctx)
	defer tr.Close()
	tr.Register(ctx, RegisterRequest{AgentID: "a1"})

	select {
	case event := <-tr.Events():
		if event.Type != "control" {
			t.Errorf("expected control, got %s", event.Type)
		}
		if event.Control.Action != "cancel" {
			t.Errorf("expected cancel, got %s", event.Control.Action)
		}
		if event.Control.TaskID != "task-99" {
			t.Errorf("expected task-99, got %s", event.Control.TaskID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for control event")
	}
}

func TestWSTransportReceiveUpgrade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		conn.ReadMessage()
		conn.WriteJSON(wsRegisteredMessage{Type: "registered", User: UserInfo{}})

		time.Sleep(50 * time.Millisecond)
		conn.WriteJSON(wsUpgradeMessage{Type: "upgrade", Version: "2.0.0"})

		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	tr := NewWSTransport(wsURL, "key", "a1", "ua")

	ctx := context.Background()
	tr.Connect(ctx)
	defer tr.Close()
	tr.Register(ctx, RegisterRequest{AgentID: "a1"})

	select {
	case event := <-tr.Events():
		if event.Type != "upgrade" {
			t.Errorf("expected upgrade, got %s", event.Type)
		}
		if event.Upgrade.Version != "2.0.0" {
			t.Errorf("expected 2.0.0, got %s", event.Upgrade.Version)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for upgrade event")
	}
}

func TestWSTransportDisconnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		conn.ReadMessage()
		conn.WriteJSON(wsRegisteredMessage{Type: "registered", User: UserInfo{}})

		// Close after a short delay to simulate disconnection
		time.Sleep(100 * time.Millisecond)
		conn.Close()
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	tr := NewWSTransport(wsURL, "key", "a1", "ua")

	ctx := context.Background()
	tr.Connect(ctx)
	defer tr.Close()
	tr.Register(ctx, RegisterRequest{AgentID: "a1"})

	select {
	case event := <-tr.Events():
		if event.Type != "disconnected" {
			t.Errorf("expected disconnected, got %s", event.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for disconnected event")
	}
}

func TestWSTransportSendProgress(t *testing.T) {
	var receivedMsg map[string]interface{}
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// Read auth
		conn.ReadMessage()
		conn.WriteJSON(wsRegisteredMessage{Type: "registered", User: UserInfo{}})

		// Read progress
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		mu.Lock()
		json.Unmarshal(msg, &receivedMsg)
		mu.Unlock()

		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	tr := NewWSTransport(wsURL, "key", "a1", "ua")

	ctx := context.Background()
	tr.Connect(ctx)
	defer tr.Close()
	tr.Register(ctx, RegisterRequest{AgentID: "a1"})

	time.Sleep(50 * time.Millisecond)
	resp, err := tr.SendProgress(ctx, StatusUpdate{
		TaskID:   "t1",
		Status:   "downloading",
		Progress: 42,
	})
	if err != nil {
		t.Fatalf("SendProgress failed: %v", err)
	}
	if !resp.Success {
		t.Error("expected success response")
	}

	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if receivedMsg["type"] != "progress" {
		t.Errorf("expected progress, got %v", receivedMsg["type"])
	}
	if receivedMsg["taskId"] != "t1" {
		t.Errorf("expected t1, got %v", receivedMsg["taskId"])
	}
	mu.Unlock()
}

// ── Hybrid Transport Tests ───────────────────────────────────────────────────

func TestHybridTransportWSSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	wsT := NewWSTransport(wsURL, "key", "a1", "ua")
	httpT := NewHTTPTransport("http://localhost", "key", "ua")

	h := NewHybridTransport(wsT, httpT)
	err := h.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer h.Close()

	if h.Mode() != "ws" {
		t.Errorf("expected ws mode, got %s", h.Mode())
	}
}

func TestHybridTransportWSFailFallbackHTTP(t *testing.T) {
	// WS URL points to nowhere
	wsT := NewWSTransport("ws://127.0.0.1:1", "key", "a1", "ua")
	httpT := NewHTTPTransport("http://localhost", "key", "ua")

	h := NewHybridTransport(wsT, httpT)
	err := h.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect should succeed with HTTP fallback: %v", err)
	}
	defer h.Close()

	if h.Mode() != "http" {
		t.Errorf("expected http mode after WS failure, got %s", h.Mode())
	}
}

func TestHybridTransportWSDisconnectSwitchesToHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Close immediately to trigger disconnect
		time.Sleep(100 * time.Millisecond)
		conn.Close()
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	wsT := NewWSTransport(wsURL, "key", "a1", "ua")
	httpT := NewHTTPTransport("http://localhost", "key", "ua")

	h := NewHybridTransport(wsT, httpT)
	h.Connect(context.Background())
	defer h.Close()

	// Wait for disconnect event
	select {
	case event := <-h.Events():
		if event.Type != "disconnected" {
			t.Errorf("expected disconnected, got %s", event.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for disconnected event")
	}

	// Mode should be HTTP now
	time.Sleep(100 * time.Millisecond)
	if h.Mode() != "http" {
		t.Errorf("expected http after disconnect, got %s", h.Mode())
	}
}
