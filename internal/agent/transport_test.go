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

// ── Additional HTTP Transport Tests ─────────────────────────────────────────

func TestNewHTTPTransportConstructor(t *testing.T) {
	tr := NewHTTPTransport("http://example.com", "my-key", "my-agent/1.0")

	if tr.client == nil {
		t.Fatal("expected client to be non-nil")
	}
	if tr.events == nil {
		t.Fatal("expected events channel to be non-nil")
	}
	// events channel should have capacity 10
	if cap(tr.events) != 10 {
		t.Errorf("expected events capacity 10, got %d", cap(tr.events))
	}
}

func TestHTTPTransportConnectAndCloseAreNoOps(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")

	if err := tr.Connect(context.Background()); err != nil {
		t.Errorf("Connect should be a no-op, got error: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("Close should be a no-op, got error: %v", err)
	}
}

func TestHTTPTransportClientAccessor(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	c := tr.Client()
	if c == nil {
		t.Fatal("Client() should return the underlying client")
	}
	if c != tr.client {
		t.Error("Client() should return the same instance stored internally")
	}
}

func TestHTTPTransportSendHeartbeat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "heartbeat") {
			t.Errorf("expected heartbeat path, got %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(HeartbeatResponse{
			Success:  true,
			Watching: true,
			Upgrade:  &UpgradeSignal{Version: "9.9.9"},
		})
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL, "key", "ua")
	resp, err := tr.SendHeartbeat(context.Background(), HeartbeatRequest{
		AgentID: "a1",
		Name:    "test",
		Version: "1.0",
	})
	if err != nil {
		t.Fatalf("SendHeartbeat failed: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}
	if !resp.Watching {
		t.Error("expected watching=true")
	}
	if resp.Upgrade == nil || resp.Upgrade.Version != "9.9.9" {
		t.Error("expected upgrade version 9.9.9")
	}
}

func TestHTTPTransportSendProgress(t *testing.T) {
	var received StatusUpdate
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(StatusResponse{
			Success:   true,
			Cancelled: true,
		})
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL, "key", "ua")
	resp, err := tr.SendProgress(context.Background(), StatusUpdate{
		TaskID:   "task-1",
		Status:   "downloading",
		Progress: 55,
		SpeedBps: 1024000,
	})
	if err != nil {
		t.Fatalf("SendProgress failed: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}
	if !resp.Cancelled {
		t.Error("expected cancelled flag")
	}
	if received.TaskID != "task-1" {
		t.Errorf("expected task-1, got %s", received.TaskID)
	}
	if received.Progress != 55 {
		t.Errorf("expected progress 55, got %d", received.Progress)
	}
}

func TestHTTPTransportClaimTasks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		agentID := r.URL.Query().Get("agentId")
		if agentID != "agent-42" {
			t.Errorf("expected agentId=agent-42, got %s", agentID)
		}
		json.NewEncoder(w).Encode(TasksResponse{
			Tasks: []Task{
				{ID: "t1", Title: "Movie 1", InfoHash: "abc"},
				{ID: "t2", Title: "Movie 2", InfoHash: "def"},
			},
		})
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL, "key", "ua")
	resp, err := tr.ClaimTasks(context.Background(), "agent-42")
	if err != nil {
		t.Fatalf("ClaimTasks failed: %v", err)
	}
	if len(resp.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(resp.Tasks))
	}
	if resp.Tasks[0].Title != "Movie 1" {
		t.Errorf("expected Movie 1, got %s", resp.Tasks[0].Title)
	}
}

func TestHTTPTransportDeregister(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		json.NewEncoder(w).Encode(StatusResponse{Success: true})
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL, "key", "ua")
	err := tr.Deregister(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("Deregister failed: %v", err)
	}
	if !called {
		t.Error("expected server to be called")
	}
}

func TestHTTPTransportBatchReportStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(BatchStatusResponse{
			Results: []StatusResponse{
				{Success: true},
				{Success: true, Cancelled: true},
			},
			Watching: true,
		})
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL, "key", "ua")
	resp, err := tr.BatchReportStatus(context.Background(), []StatusUpdate{
		{TaskID: "t1", Status: "downloading", Progress: 10},
		{TaskID: "t2", Status: "completed", Progress: 100},
	})
	if err != nil {
		t.Fatalf("BatchReportStatus failed: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Results))
	}
	if !resp.Watching {
		t.Error("expected watching=true")
	}
	if !resp.Results[1].Cancelled {
		t.Error("expected second result to be cancelled")
	}
}

func TestHTTPTransportAuthHeader(t *testing.T) {
	var gotAuth string
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		json.NewEncoder(w).Encode(RegisterResponse{Success: true})
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL, "secret-key-123", "unarr/2.0")
	tr.Register(context.Background(), RegisterRequest{AgentID: "a1"})

	if gotAuth != "Bearer secret-key-123" {
		t.Errorf("expected Bearer secret-key-123, got %s", gotAuth)
	}
	if gotUA != "unarr/2.0" {
		t.Errorf("expected unarr/2.0, got %s", gotUA)
	}
}

// ── Additional WebSocket Transport Tests ────────────────────────────────────

func TestNewWSTransportConstructor(t *testing.T) {
	tr := NewWSTransport("ws://example.com/ws", "api-key", "agent-1", "ua/1.0")

	if tr.Mode() != "ws" {
		t.Errorf("expected ws mode, got %s", tr.Mode())
	}
	if tr.wsURL != "ws://example.com/ws" {
		t.Errorf("expected ws URL, got %s", tr.wsURL)
	}
	if tr.apiKey != "api-key" {
		t.Errorf("expected api-key, got %s", tr.apiKey)
	}
	if tr.agentID != "agent-1" {
		t.Errorf("expected agent-1, got %s", tr.agentID)
	}
	if tr.userAgent != "ua/1.0" {
		t.Errorf("expected ua/1.0, got %s", tr.userAgent)
	}
	if cap(tr.events) != 50 {
		t.Errorf("expected events capacity 50, got %d", cap(tr.events))
	}
	if tr.authDone == nil {
		t.Fatal("expected authDone channel to be non-nil")
	}
}

func TestWSTransportClaimTasksIsNoOp(t *testing.T) {
	tr := NewWSTransport("ws://localhost", "key", "a1", "ua")
	resp, err := tr.ClaimTasks(context.Background(), "a1")
	if err != nil {
		t.Fatalf("ClaimTasks should succeed (no-op): %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if len(resp.Tasks) != 0 {
		t.Errorf("expected 0 tasks, got %d", len(resp.Tasks))
	}
}

func TestWSTransportCloseWhenNotConnected(t *testing.T) {
	tr := NewWSTransport("ws://localhost", "key", "a1", "ua")
	// Close without ever connecting should not panic or error
	if err := tr.Close(); err != nil {
		t.Errorf("Close on unconnected transport should return nil, got %v", err)
	}
}

func TestWSTransportSendWhenNotConnected(t *testing.T) {
	tr := NewWSTransport("ws://localhost", "key", "a1", "ua")
	// Attempting to send a heartbeat without connecting should fail
	_, err := tr.SendHeartbeat(context.Background(), HeartbeatRequest{AgentID: "a1"})
	if err == nil {
		t.Error("expected error when sending without connection")
	}
}

func TestWSTransportConnectBadURL(t *testing.T) {
	tr := NewWSTransport("ws://127.0.0.1:1", "key", "a1", "ua")
	err := tr.Connect(context.Background())
	if err == nil {
		t.Error("expected error connecting to invalid address")
	}
}

func TestWSTransportSendHeartbeatWithDisk(t *testing.T) {
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

		// Read heartbeat
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
	resp, err := tr.SendHeartbeat(ctx, HeartbeatRequest{
		AgentID:        "a1",
		DiskFreeBytes:  500000000,
		DiskTotalBytes: 1000000000,
	})
	if err != nil {
		t.Fatalf("SendHeartbeat failed: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}

	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if receivedMsg["type"] != "heartbeat" {
		t.Errorf("expected heartbeat, got %v", receivedMsg["type"])
	}
	disk, ok := receivedMsg["disk"].(map[string]interface{})
	if !ok {
		t.Fatal("expected disk field in heartbeat message")
	}
	if disk["free"].(float64) != 500000000 {
		t.Errorf("expected free=500000000, got %v", disk["free"])
	}
	if disk["total"].(float64) != 1000000000 {
		t.Errorf("expected total=1000000000, got %v", disk["total"])
	}
}

func TestWSTransportSendHeartbeatWithoutDisk(t *testing.T) {
	var receivedMsg map[string]interface{}
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		conn.ReadMessage()
		conn.WriteJSON(wsRegisteredMessage{Type: "registered", User: UserInfo{}})

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
	resp, err := tr.SendHeartbeat(ctx, HeartbeatRequest{AgentID: "a1"})
	if err != nil {
		t.Fatalf("SendHeartbeat failed: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}

	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if receivedMsg["type"] != "heartbeat" {
		t.Errorf("expected heartbeat, got %v", receivedMsg["type"])
	}
	// disk field should be absent when no disk info provided
	if _, exists := receivedMsg["disk"]; exists {
		t.Error("expected no disk field when disk info is zero")
	}
}

func TestWSTransportDeregisterClosesConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.ReadMessage()
		conn.WriteJSON(wsRegisteredMessage{Type: "registered", User: UserInfo{}})
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	tr := NewWSTransport(wsURL, "key", "a1", "ua")

	ctx := context.Background()
	tr.Connect(ctx)
	tr.Register(ctx, RegisterRequest{AgentID: "a1"})

	err := tr.Deregister(ctx, "a1")
	if err != nil {
		t.Fatalf("Deregister failed: %v", err)
	}

	// After deregister, send should fail (connection closed)
	_, err = tr.SendHeartbeat(ctx, HeartbeatRequest{AgentID: "a1"})
	if err == nil {
		t.Error("expected error sending after deregister")
	}
}

func TestWSTransportReceiveStreamRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		conn.ReadMessage()
		conn.WriteJSON(wsRegisteredMessage{Type: "registered", User: UserInfo{}})

		time.Sleep(50 * time.Millisecond)
		conn.WriteJSON(wsTasksMessage{
			Type:  "tasks",
			Tasks: []Task{},
			StreamRequests: []StreamRequest{
				{TaskID: "t1", FilePath: "/data/movie.mkv"},
			},
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
		if event.Type != "tasks" {
			t.Errorf("expected tasks, got %s", event.Type)
		}
		if len(event.Tasks.StreamRequests) != 1 {
			t.Fatalf("expected 1 stream request, got %d", len(event.Tasks.StreamRequests))
		}
		if event.Tasks.StreamRequests[0].FilePath != "/data/movie.mkv" {
			t.Errorf("expected /data/movie.mkv, got %s", event.Tasks.StreamRequests[0].FilePath)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for tasks event with stream requests")
	}
}

func TestWSTransportReceiveErrorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		conn.ReadMessage()
		conn.WriteJSON(wsRegisteredMessage{Type: "registered", User: UserInfo{}})

		time.Sleep(50 * time.Millisecond)
		// Send an error message (should be logged, not emitted as event)
		conn.WriteJSON(map[string]string{
			"type":    "error",
			"message": "rate limited",
		})

		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	tr := NewWSTransport(wsURL, "key", "a1", "ua")

	ctx := context.Background()
	tr.Connect(ctx)
	defer tr.Close()
	tr.Register(ctx, RegisterRequest{AgentID: "a1"})

	// Error messages are logged but not emitted — events channel should be quiet
	select {
	case event := <-tr.Events():
		// If we get disconnected, that's acceptable (server closes after delay)
		if event.Type != "disconnected" {
			t.Errorf("unexpected event type: %s", event.Type)
		}
	case <-time.After(300 * time.Millisecond):
		// Expected: no event emitted for error messages
	}
}

func TestWSTransportRegisterTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.ReadMessage()
		// Never send registered response — should timeout
		time.Sleep(20 * time.Second)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	tr := NewWSTransport(wsURL, "key", "a1", "ua")

	ctx := context.Background()
	tr.Connect(ctx)
	defer tr.Close()

	// Use a context with short timeout to avoid waiting 15s
	ctxShort, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	_, err := tr.Register(ctxShort, RegisterRequest{AgentID: "a1"})
	if err == nil {
		t.Error("expected timeout error from Register")
	}
}

// ── Additional Hybrid Transport Tests ───────────────────────────────────────

func TestNewHybridTransportConstructor(t *testing.T) {
	wsT := NewWSTransport("ws://localhost", "key", "a1", "ua")
	httpT := NewHTTPTransport("http://localhost", "key", "ua")

	h := NewHybridTransport(wsT, httpT)

	if h.Mode() != "http" {
		t.Errorf("expected initial mode http, got %s", h.Mode())
	}
	if cap(h.events) != 50 {
		t.Errorf("expected events capacity 50, got %d", cap(h.events))
	}
	if h.ws != wsT {
		t.Error("expected ws transport to match")
	}
	if h.http != httpT {
		t.Error("expected http transport to match")
	}
	if h.reconnectStop == nil {
		t.Error("expected reconnectStop channel to be non-nil")
	}
}

func TestHybridTransportCloseIsIdempotent(t *testing.T) {
	wsT := NewWSTransport("ws://127.0.0.1:1", "key", "a1", "ua")
	httpT := NewHTTPTransport("http://localhost", "key", "ua")

	h := NewHybridTransport(wsT, httpT)
	// Close twice should not panic
	if err := h.Close(); err != nil {
		t.Errorf("first Close failed: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("second Close failed: %v", err)
	}
}

func TestHybridTransportHTTPModeRegister(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(RegisterResponse{
			Success: true,
			User:    UserInfo{Name: "HTTPUser", Plan: "free"},
		})
	}))
	defer srv.Close()

	wsT := NewWSTransport("ws://127.0.0.1:1", "key", "a1", "ua")
	httpT := NewHTTPTransport(srv.URL, "key", "ua")

	h := NewHybridTransport(wsT, httpT)
	// Force HTTP mode (default)
	h.mode.Store("http")

	resp, err := h.Register(context.Background(), RegisterRequest{AgentID: "a1"})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if resp.User.Name != "HTTPUser" {
		t.Errorf("expected HTTPUser, got %s", resp.User.Name)
	}
}

func TestHybridTransportHTTPModeClaimTasks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(TasksResponse{
			Tasks: []Task{{ID: "t1", Title: "Test"}},
		})
	}))
	defer srv.Close()

	wsT := NewWSTransport("ws://127.0.0.1:1", "key", "a1", "ua")
	httpT := NewHTTPTransport(srv.URL, "key", "ua")

	h := NewHybridTransport(wsT, httpT)
	h.mode.Store("http")

	resp, err := h.ClaimTasks(context.Background(), "a1")
	if err != nil {
		t.Fatalf("ClaimTasks failed: %v", err)
	}
	if len(resp.Tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(resp.Tasks))
	}
}

func TestHybridTransportHTTPModeDeregister(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(StatusResponse{Success: true})
	}))
	defer srv.Close()

	wsT := NewWSTransport("ws://127.0.0.1:1", "key", "a1", "ua")
	httpT := NewHTTPTransport(srv.URL, "key", "ua")

	h := NewHybridTransport(wsT, httpT)
	h.mode.Store("http")

	err := h.Deregister(context.Background(), "a1")
	if err != nil {
		t.Fatalf("Deregister failed: %v", err)
	}
}

func TestHybridTransportHTTPModeSendHeartbeat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(HeartbeatResponse{Success: true, Watching: true})
	}))
	defer srv.Close()

	wsT := NewWSTransport("ws://127.0.0.1:1", "key", "a1", "ua")
	httpT := NewHTTPTransport(srv.URL, "key", "ua")

	h := NewHybridTransport(wsT, httpT)
	h.mode.Store("http")

	resp, err := h.SendHeartbeat(context.Background(), HeartbeatRequest{AgentID: "a1"})
	if err != nil {
		t.Fatalf("SendHeartbeat failed: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}
	if !resp.Watching {
		t.Error("expected watching=true")
	}
}

func TestHybridTransportHTTPModeSendProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(StatusResponse{Success: true})
	}))
	defer srv.Close()

	wsT := NewWSTransport("ws://127.0.0.1:1", "key", "a1", "ua")
	httpT := NewHTTPTransport(srv.URL, "key", "ua")

	h := NewHybridTransport(wsT, httpT)
	h.mode.Store("http")

	resp, err := h.SendProgress(context.Background(), StatusUpdate{
		TaskID:   "t1",
		Status:   "completed",
		Progress: 100,
	})
	if err != nil {
		t.Fatalf("SendProgress failed: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}
}

func TestHybridTransportWSModeClaimTasksIsNoOp(t *testing.T) {
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
	h.Connect(context.Background())
	defer h.Close()

	// In WS mode, ClaimTasks delegates to WS which is a no-op
	resp, err := h.ClaimTasks(context.Background(), "a1")
	if err != nil {
		t.Fatalf("ClaimTasks failed: %v", err)
	}
	if len(resp.Tasks) != 0 {
		t.Errorf("expected 0 tasks in WS mode, got %d", len(resp.Tasks))
	}
}

func TestHybridTransportEventsChannel(t *testing.T) {
	wsT := NewWSTransport("ws://127.0.0.1:1", "key", "a1", "ua")
	httpT := NewHTTPTransport("http://localhost", "key", "ua")

	h := NewHybridTransport(wsT, httpT)
	ch := h.Events()
	if ch == nil {
		t.Fatal("Events() should return non-nil channel")
	}
	// Verify it is the correct channel
	if cap(ch) != 50 {
		t.Errorf("expected events capacity 50, got %d", cap(ch))
	}
}

func TestHybridTransportSwitchToHTTPIdempotent(t *testing.T) {
	wsT := NewWSTransport("ws://127.0.0.1:1", "key", "a1", "ua")
	httpT := NewHTTPTransport("http://localhost", "key", "ua")

	h := NewHybridTransport(wsT, httpT)
	// Already in HTTP mode, switchToHTTP should be a no-op
	h.mode.Store("http")
	h.switchToHTTP() // should not panic or start reconnect

	if h.Mode() != "http" {
		t.Errorf("expected http, got %s", h.Mode())
	}
}

// ── Daemon Constructor & Utility Tests ──────────────────────────────────────

func TestNewDaemonDefaults(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	d := NewDaemon(DaemonConfig{
		AgentID:     "a1",
		AgentName:   "test",
		Version:     "1.0",
		DownloadDir: "/tmp",
	}, tr)

	if d.cfg.PollInterval != 30*time.Second {
		t.Errorf("expected default PollInterval 30s, got %v", d.cfg.PollInterval)
	}
	if d.cfg.HeartbeatInterval != 30*time.Second {
		t.Errorf("expected default HeartbeatInterval 30s, got %v", d.cfg.HeartbeatInterval)
	}
	if d.Transport() != tr {
		t.Error("Transport() should return the configured transport")
	}
	if d.pollNow == nil {
		t.Error("pollNow channel should be initialized")
	}
}

func TestNewDaemonCustomIntervals(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	d := NewDaemon(DaemonConfig{
		AgentID:           "a1",
		AgentName:         "test",
		Version:           "1.0",
		DownloadDir:       "/tmp",
		PollInterval:      10 * time.Second,
		HeartbeatInterval: 15 * time.Second,
	}, tr)

	if d.cfg.PollInterval != 10*time.Second {
		t.Errorf("expected PollInterval 10s, got %v", d.cfg.PollInterval)
	}
	if d.cfg.HeartbeatInterval != 15*time.Second {
		t.Errorf("expected HeartbeatInterval 15s, got %v", d.cfg.HeartbeatInterval)
	}
}

func TestDaemonTriggerPoll(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	d := NewDaemon(DaemonConfig{
		AgentID:     "a1",
		AgentName:   "test",
		Version:     "1.0",
		DownloadDir: "/tmp",
	}, tr)

	// First trigger should succeed
	d.TriggerPoll()

	// Channel should have one signal
	select {
	case <-d.pollNow:
		// good
	default:
		t.Error("expected signal on pollNow channel")
	}

	// Second trigger when channel is empty should also succeed
	d.TriggerPoll()
	select {
	case <-d.pollNow:
		// good
	default:
		t.Error("expected signal on pollNow channel after second trigger")
	}
}

func TestDaemonTriggerPollNonBlocking(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	d := NewDaemon(DaemonConfig{
		AgentID:     "a1",
		AgentName:   "test",
		Version:     "1.0",
		DownloadDir: "/tmp",
	}, tr)

	// Fill the channel (capacity 1)
	d.TriggerPoll()
	// Second call should not block even though channel is full
	done := make(chan struct{})
	go func() {
		d.TriggerPoll()
		close(done)
	}()

	select {
	case <-done:
		// good, did not block
	case <-time.After(1 * time.Second):
		t.Fatal("TriggerPoll blocked on full channel")
	}
}

func TestDaemonHandleEventTasks(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	d := NewDaemon(DaemonConfig{
		AgentID:     "a1",
		AgentName:   "test",
		Version:     "1.0",
		DownloadDir: "/tmp",
	}, tr)

	var claimedTasks []Task
	d.OnTasksClaimed = func(tasks []Task) {
		claimedTasks = tasks
	}

	d.handleEvent(ServerEvent{
		Type: "tasks",
		Tasks: &TasksResponse{
			Tasks: []Task{
				{ID: "t1", Title: "Movie 1"},
				{ID: "t2", Title: "Movie 2"},
			},
		},
	})

	if len(claimedTasks) != 2 {
		t.Fatalf("expected 2 claimed tasks, got %d", len(claimedTasks))
	}
	if claimedTasks[0].Title != "Movie 1" {
		t.Errorf("expected Movie 1, got %s", claimedTasks[0].Title)
	}
}

func TestDaemonHandleEventTasksWithStreamRequests(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	d := NewDaemon(DaemonConfig{
		AgentID:     "a1",
		AgentName:   "test",
		Version:     "1.0",
		DownloadDir: "/tmp",
	}, tr)

	var streamReqs []StreamRequest
	d.OnStreamRequested = func(req StreamRequest) {
		streamReqs = append(streamReqs, req)
	}

	d.handleEvent(ServerEvent{
		Type: "tasks",
		Tasks: &TasksResponse{
			Tasks: []Task{},
			StreamRequests: []StreamRequest{
				{TaskID: "t1", FilePath: "/data/movie.mkv"},
				{TaskID: "t2", FilePath: "/data/show.mkv"},
			},
		},
	})

	if len(streamReqs) != 2 {
		t.Fatalf("expected 2 stream requests, got %d", len(streamReqs))
	}
	if streamReqs[0].FilePath != "/data/movie.mkv" {
		t.Errorf("expected /data/movie.mkv, got %s", streamReqs[0].FilePath)
	}
}

func TestDaemonHandleEventUpgrade(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	d := NewDaemon(DaemonConfig{
		AgentID:     "a1",
		AgentName:   "test",
		Version:     "1.0",
		DownloadDir: "/tmp",
	}, tr)

	d.handleEvent(ServerEvent{
		Type:    "upgrade",
		Upgrade: &UpgradeSignal{Version: "2.0.0"},
	})

	if d.lastNotifiedVersion != "2.0.0" {
		t.Errorf("expected lastNotifiedVersion 2.0.0, got %s", d.lastNotifiedVersion)
	}

	// Same version again should not update (already notified)
	d.lastNotifiedVersion = "2.0.0"
	d.handleEvent(ServerEvent{
		Type:    "upgrade",
		Upgrade: &UpgradeSignal{Version: "2.0.0"},
	})
	// Still 2.0.0, no change
	if d.lastNotifiedVersion != "2.0.0" {
		t.Errorf("expected lastNotifiedVersion unchanged at 2.0.0, got %s", d.lastNotifiedVersion)
	}
}

func TestDaemonHandleEventControl(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	d := NewDaemon(DaemonConfig{
		AgentID:     "a1",
		AgentName:   "test",
		Version:     "1.0",
		DownloadDir: "/tmp",
	}, tr)

	var gotAction, gotTaskID string
	d.OnControlAction = func(action, taskID string) {
		gotAction = action
		gotTaskID = taskID
	}

	d.handleEvent(ServerEvent{
		Type:    "control",
		Control: &ControlAction{Action: "cancel", TaskID: "task-99"},
	})

	if gotAction != "cancel" {
		t.Errorf("expected cancel, got %s", gotAction)
	}
	if gotTaskID != "task-99" {
		t.Errorf("expected task-99, got %s", gotTaskID)
	}
}

func TestDaemonHandleEventControlWithNilCallback(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	d := NewDaemon(DaemonConfig{
		AgentID:     "a1",
		AgentName:   "test",
		Version:     "1.0",
		DownloadDir: "/tmp",
	}, tr)

	// OnControlAction is nil — should not panic
	d.handleEvent(ServerEvent{
		Type:    "control",
		Control: &ControlAction{Action: "pause", TaskID: "t1"},
	})
}

func TestDaemonHandleEventDisconnected(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	d := NewDaemon(DaemonConfig{
		AgentID:     "a1",
		AgentName:   "test",
		Version:     "1.0",
		DownloadDir: "/tmp",
	}, tr)

	// disconnected event should not panic (just logs)
	d.handleEvent(ServerEvent{Type: "disconnected"})
}

func TestDaemonHandleEventTasksNilCallback(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	d := NewDaemon(DaemonConfig{
		AgentID:     "a1",
		AgentName:   "test",
		Version:     "1.0",
		DownloadDir: "/tmp",
	}, tr)

	// OnTasksClaimed is nil — should not panic
	d.handleEvent(ServerEvent{
		Type: "tasks",
		Tasks: &TasksResponse{
			Tasks: []Task{{ID: "t1", Title: "Test"}},
		},
	})
}

func TestDaemonHandleEventEmptyTasks(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	d := NewDaemon(DaemonConfig{
		AgentID:     "a1",
		AgentName:   "test",
		Version:     "1.0",
		DownloadDir: "/tmp",
	}, tr)

	var called bool
	d.OnTasksClaimed = func(tasks []Task) {
		called = true
	}

	// Empty tasks should not trigger callback
	d.handleEvent(ServerEvent{
		Type:  "tasks",
		Tasks: &TasksResponse{Tasks: []Task{}},
	})

	if called {
		t.Error("OnTasksClaimed should not be called for empty task list")
	}
}

func TestDaemonHandleEventNilTasks(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	d := NewDaemon(DaemonConfig{
		AgentID:     "a1",
		AgentName:   "test",
		Version:     "1.0",
		DownloadDir: "/tmp",
	}, tr)

	// Nil Tasks field should not panic
	d.handleEvent(ServerEvent{
		Type:  "tasks",
		Tasks: nil,
	})
}

func TestDaemonHandleEventUpgradeNilSignal(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	d := NewDaemon(DaemonConfig{
		AgentID:     "a1",
		AgentName:   "test",
		Version:     "1.0",
		DownloadDir: "/tmp",
	}, tr)

	// Nil Upgrade should not panic
	d.handleEvent(ServerEvent{
		Type:    "upgrade",
		Upgrade: nil,
	})
	if d.lastNotifiedVersion != "" {
		t.Errorf("expected empty lastNotifiedVersion, got %s", d.lastNotifiedVersion)
	}
}

func TestDaemonHandleEventUpgradeEmptyVersion(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	d := NewDaemon(DaemonConfig{
		AgentID:     "a1",
		AgentName:   "test",
		Version:     "1.0",
		DownloadDir: "/tmp",
	}, tr)

	// Empty version should not update lastNotifiedVersion
	d.handleEvent(ServerEvent{
		Type:    "upgrade",
		Upgrade: &UpgradeSignal{Version: ""},
	})
	if d.lastNotifiedVersion != "" {
		t.Errorf("expected empty lastNotifiedVersion, got %s", d.lastNotifiedVersion)
	}
}

func TestDaemonWatchingFlag(t *testing.T) {
	tr := NewHTTPTransport("http://localhost", "key", "ua")
	d := NewDaemon(DaemonConfig{
		AgentID:     "a1",
		AgentName:   "test",
		Version:     "1.0",
		DownloadDir: "/tmp",
	}, tr)

	if d.Watching.Load() {
		t.Error("expected Watching to be false initially")
	}
	d.Watching.Store(true)
	if !d.Watching.Load() {
		t.Error("expected Watching to be true after Store(true)")
	}
}

// ── Transport Interface Compliance ──────────────────────────────────────────

func TestHTTPTransportImplementsTransport(t *testing.T) {
	var _ Transport = (*HTTPTransport)(nil)
}

func TestWSTransportImplementsTransport(t *testing.T) {
	var _ Transport = (*WSTransport)(nil)
}

func TestHybridTransportImplementsTransport(t *testing.T) {
	var _ Transport = (*HybridTransport)(nil)
}
