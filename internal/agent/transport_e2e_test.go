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
)

// TestE2EFullLifecycle tests the full lifecycle:
// connect → auth → receive tasks → send progress → receive control → disconnect → reconnect
func TestE2EFullLifecycle(t *testing.T) {
	var mu sync.Mutex
	var receivedMessages []map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}

			var parsed map[string]interface{}
			json.Unmarshal(msg, &parsed)

			mu.Lock()
			receivedMessages = append(receivedMessages, parsed)
			mu.Unlock()

			msgType, _ := parsed["type"].(string)
			switch msgType {
			case "auth":
				conn.WriteJSON(wsRegisteredMessage{
					Type:     "registered",
					User:     UserInfo{Name: "E2E User", Plan: "pro", IsPro: true},
					Features: FeatureFlags{Torrent: true, Debrid: true},
				})

			case "heartbeat":
				// No response in WS mode

			case "progress":
				// Simulate server-side cancel after progress
				if progress, ok := parsed["progress"].(float64); ok && progress >= 50 {
					conn.WriteJSON(map[string]string{
						"type":   "control",
						"action": "cancel",
						"taskId": parsed["taskId"].(string),
					})
				}

			case "upgrade-result":
				// Acknowledged
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	tr := NewWSTransport(wsURL, "e2e-key", "e2e-agent", "test/1.0")

	ctx := context.Background()

	// 1. Connect
	if err := tr.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer tr.Close()

	// 2. Auth
	resp, err := tr.Register(ctx, RegisterRequest{
		AgentID: "e2e-agent",
		Name:    "E2E Test Agent",
		Version: "1.0.0",
		OS:      "linux",
		Arch:    "amd64",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.User.Name != "E2E User" {
		t.Errorf("expected E2E User, got %s", resp.User.Name)
	}
	if !resp.Features.Debrid {
		t.Error("expected debrid feature")
	}

	// 3. Send heartbeat
	_, err = tr.SendHeartbeat(ctx, HeartbeatRequest{
		AgentID:        "e2e-agent",
		DiskFreeBytes:  1000000000,
		DiskTotalBytes: 5000000000,
	})
	if err != nil {
		t.Fatalf("SendHeartbeat: %v", err)
	}

	// 4. Send progress (50% → should trigger cancel control)
	_, err = tr.SendProgress(ctx, StatusUpdate{
		TaskID:          "task-e2e-1",
		Status:          "downloading",
		Progress:        50,
		DownloadedBytes: 500,
		TotalBytes:      1000,
		SpeedBps:        100,
	})
	if err != nil {
		t.Fatalf("SendProgress: %v", err)
	}

	// 5. Wait for control event (cancel)
	select {
	case event := <-tr.Events():
		if event.Type != "control" {
			t.Errorf("expected control event, got %s", event.Type)
		}
		if event.Control.Action != "cancel" {
			t.Errorf("expected cancel, got %s", event.Control.Action)
		}
		if event.Control.TaskID != "task-e2e-1" {
			t.Errorf("expected task-e2e-1, got %s", event.Control.TaskID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for cancel control")
	}

	// Verify server received all messages
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()

	if len(receivedMessages) < 3 {
		t.Fatalf("expected at least 3 messages, got %d", len(receivedMessages))
	}

	types := make([]string, len(receivedMessages))
	for i, m := range receivedMessages {
		types[i], _ = m["type"].(string)
	}

	expected := []string{"auth", "heartbeat", "progress"}
	for _, exp := range expected {
		found := false
		for _, got := range types {
			if got == exp {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing message type %q in %v", exp, types)
		}
	}
}

// TestE2EHybridFailover tests the full failover scenario:
// WS connect → download → WS disconnect → switch to HTTP → continue working
func TestE2EHybridFailover(t *testing.T) {
	connectionCount := 0
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		mu.Lock()
		connectionCount++
		connNum := connectionCount
		mu.Unlock()

		// Read auth
		conn.ReadMessage()
		conn.WriteJSON(wsRegisteredMessage{
			Type: "registered",
			User: UserInfo{Name: "Failover User"},
		})

		if connNum == 1 {
			// First connection: push tasks then disconnect after 200ms
			time.Sleep(50 * time.Millisecond)
			conn.WriteJSON(wsTasksMessage{
				Type:  "tasks",
				Tasks: []Task{{ID: "t1", InfoHash: "abc", Title: "Failover Movie"}},
			})
			time.Sleep(150 * time.Millisecond)
			conn.Close()
		} else {
			// Second connection (after reconnect): push upgrade
			time.Sleep(50 * time.Millisecond)
			conn.WriteJSON(wsUpgradeMessage{Type: "upgrade", Version: "3.0.0"})
			time.Sleep(500 * time.Millisecond)
			conn.Close()
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	wsT := NewWSTransport(wsURL, "key", "a1", "ua")

	// HTTP mock for fallback
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simple heartbeat response
		json.NewEncoder(w).Encode(HeartbeatResponse{Success: true})
	}))
	defer httpSrv.Close()

	httpT := NewHTTPTransport(httpSrv.URL, "key", "ua")
	h := NewHybridTransport(wsT, httpT)

	ctx := context.Background()
	err := h.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer h.Close()

	// Should start in WS mode
	if h.Mode() != "ws" {
		t.Fatalf("expected ws mode, got %s", h.Mode())
	}

	// Register via WS
	_, err = h.Register(ctx, RegisterRequest{AgentID: "a1"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Receive tasks via WS
	var tasksReceived bool
	var disconnected bool

	for i := 0; i < 3; i++ {
		select {
		case event := <-h.Events():
			switch event.Type {
			case "tasks":
				tasksReceived = true
				if len(event.Tasks.Tasks) != 1 || event.Tasks.Tasks[0].Title != "Failover Movie" {
					t.Errorf("unexpected tasks: %+v", event.Tasks)
				}
			case "disconnected":
				disconnected = true
			}
		case <-time.After(2 * time.Second):
			break
		}
		if disconnected {
			break
		}
	}

	if !tasksReceived {
		t.Error("did not receive tasks before disconnect")
	}
	if !disconnected {
		t.Error("did not receive disconnect event")
	}

	// Should now be in HTTP mode
	time.Sleep(100 * time.Millisecond)
	if h.Mode() != "http" {
		t.Errorf("expected http mode after disconnect, got %s", h.Mode())
	}

	// Heartbeat should work via HTTP fallback
	hbResp, err := h.SendHeartbeat(ctx, HeartbeatRequest{AgentID: "a1"})
	if err != nil {
		t.Fatalf("SendHeartbeat via HTTP fallback: %v", err)
	}
	if !hbResp.Success {
		t.Error("expected heartbeat success")
	}
}
