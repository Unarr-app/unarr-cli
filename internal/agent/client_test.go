package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegister(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/internal/agent/register" {
			t.Errorf("path = %s, want /api/internal/agent/register", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth = %q, want Bearer test-key", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %q, want application/json", r.Header.Get("Content-Type"))
		}

		var req RegisterRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.AgentID != "agent-123" {
			t.Errorf("agentId = %q, want agent-123", req.AgentID)
		}

		json.NewEncoder(w).Encode(RegisterResponse{
			Success: true,
			User:    UserInfo{Name: "David", Email: "d@test.com", Plan: "pro", IsPro: true},
			Features: FeatureFlags{
				Debrid:  true,
				Usenet:  false,
				Torrent: true,
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	resp, err := c.Register(context.Background(), RegisterRequest{
		AgentID: "agent-123",
		Name:    "Test Machine",
		OS:      "linux",
		Arch:    "amd64",
		Version: "0.2.0",
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
	if resp.User.Name != "David" {
		t.Errorf("user.name = %q, want David", resp.User.Name)
	}
	if !resp.User.IsPro {
		t.Error("expected IsPro=true")
	}
	if !resp.Features.Debrid {
		t.Error("expected debrid=true")
	}
	if !resp.Features.Torrent {
		t.Error("expected torrent=true")
	}
	if resp.Features.Usenet {
		t.Error("expected usenet=false")
	}
}

func TestHeartbeat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/heartbeat" {
			t.Errorf("path = %s, want /api/internal/agent/heartbeat", r.URL.Path)
		}
		var req HeartbeatRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.AgentID != "agent-123" {
			t.Errorf("agentId = %q, want agent-123", req.AgentID)
		}
		json.NewEncoder(w).Encode(StatusResponse{Success: true})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	err := c.Heartbeat(context.Background(), HeartbeatRequest{AgentID: "agent-123"})
	if err != nil {
		t.Fatalf("Heartbeat failed: %v", err)
	}
}

func TestClaimTasks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Query().Get("agentId") != "agent-123" {
			t.Errorf("agentId param = %q, want agent-123", r.URL.Query().Get("agentId"))
		}
		json.NewEncoder(w).Encode(TasksResponse{
			Tasks: []Task{
				{
					ID:              "task-uuid-1",
					InfoHash:        "abc123def456abc123def456abc123def456abc1",
					Title:           "The Matrix (1999)",
					PreferredMethod: "auto",
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	tasks, err := c.ClaimTasks(context.Background(), "agent-123")
	if err != nil {
		t.Fatalf("ClaimTasks failed: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	if tasks[0].ID != "task-uuid-1" {
		t.Errorf("task.ID = %q, want task-uuid-1", tasks[0].ID)
	}
	if tasks[0].InfoHash != "abc123def456abc123def456abc123def456abc1" {
		t.Errorf("task.InfoHash = %q", tasks[0].InfoHash)
	}
	if tasks[0].PreferredMethod != "auto" {
		t.Errorf("task.PreferredMethod = %q, want auto", tasks[0].PreferredMethod)
	}
}

func TestReportStatus(t *testing.T) {
	var received StatusUpdate
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/status" {
			t.Errorf("path = %s, want /api/internal/agent/status", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(StatusResponse{Success: true})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	_, err := c.ReportStatus(context.Background(), StatusUpdate{
		TaskID:          "task-uuid-1",
		Status:          "downloading",
		Progress:        42,
		DownloadedBytes: 1073741824,
		TotalBytes:      2147483648,
		SpeedBps:        5242880,
		ETA:             120,
		ResolvedMethod:  "torrent",
		FileName:        "The.Matrix.1999.1080p.mkv",
	})
	if err != nil {
		t.Fatalf("ReportStatus failed: %v", err)
	}
	if received.TaskID != "task-uuid-1" {
		t.Errorf("taskId = %q, want task-uuid-1", received.TaskID)
	}
	if received.Progress != 42 {
		t.Errorf("progress = %d, want 42", received.Progress)
	}
	if received.ResolvedMethod != "torrent" {
		t.Errorf("resolvedMethod = %q, want torrent", received.ResolvedMethod)
	}
}

func TestClaimTasksEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(TasksResponse{Tasks: []Task{}})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	tasks, err := c.ClaimTasks(context.Background(), "agent-123")
	if err != nil {
		t.Fatalf("ClaimTasks failed: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected empty tasks, got %d", len(tasks))
	}
}

func TestAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(ErrorResponse{Error: "Invalid API key"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "bad-key", "unarr-test")
	_, err := c.Register(context.Background(), RegisterRequest{AgentID: "x"})
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if got := err.Error(); got == "" {
		t.Error("error message should not be empty")
	}
}

func TestAPIError404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(ErrorResponse{Error: "Task not found"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	_, err := c.ReportStatus(context.Background(), StatusUpdate{TaskID: "missing"})
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestReportStatusCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(StatusResponse{Success: true, Cancelled: true})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	resp, err := c.ReportStatus(context.Background(), StatusUpdate{TaskID: "task-1", Status: "downloading"})
	if err != nil {
		t.Fatalf("ReportStatus failed: %v", err)
	}
	if !resp.Cancelled {
		t.Error("expected cancelled=true")
	}
}

func TestReportStatusPaused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(StatusResponse{Success: true, Paused: true})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	resp, err := c.ReportStatus(context.Background(), StatusUpdate{TaskID: "task-1", Status: "downloading"})
	if err != nil {
		t.Fatalf("ReportStatus failed: %v", err)
	}
	if !resp.Paused {
		t.Error("expected paused=true")
	}
	if resp.Cancelled {
		t.Error("expected cancelled=false")
	}
}

func TestReportStatusDeleteFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(StatusResponse{Success: true, Cancelled: true, DeleteFiles: true})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	resp, err := c.ReportStatus(context.Background(), StatusUpdate{TaskID: "task-1"})
	if err != nil {
		t.Fatalf("ReportStatus failed: %v", err)
	}
	if !resp.Cancelled {
		t.Error("expected cancelled=true")
	}
	if !resp.DeleteFiles {
		t.Error("expected deleteFiles=true")
	}
}

func TestUserAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "unarr/0.2.0" {
			t.Errorf("User-Agent = %q, want unarr/0.2.0", r.Header.Get("User-Agent"))
		}
		json.NewEncoder(w).Encode(StatusResponse{Success: true})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr/0.2.0")
	c.Heartbeat(context.Background(), HeartbeatRequest{AgentID: "x"})
}
