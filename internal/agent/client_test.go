package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
		json.NewEncoder(w).Encode(RegisterResponse{Success: true})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr/0.2.0")
	c.Register(context.Background(), RegisterRequest{AgentID: "x"})
}

func TestDeregister(t *testing.T) {
	var received struct {
		AgentID string `json:"agentId"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/deregister" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(StatusResponse{Success: true})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	err := c.Deregister(context.Background(), "agent-42")
	if err != nil {
		t.Fatalf("Deregister failed: %v", err)
	}
	if received.AgentID != "agent-42" {
		t.Errorf("agentId = %q, want agent-42", received.AgentID)
	}
}

func TestBatchReportStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/status" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var req BatchStatusRequest
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Updates) != 2 {
			t.Errorf("expected 2 updates, got %d", len(req.Updates))
		}
		json.NewEncoder(w).Encode(BatchStatusResponse{
			Results: []StatusResponse{
				{Success: true},
				{Success: true, Cancelled: true},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	resp, err := c.BatchReportStatus(context.Background(), []StatusUpdate{
		{TaskID: "t1", Status: "downloading"},
		{TaskID: "t2", Status: "completed"},
	})
	if err != nil {
		t.Fatalf("BatchReportStatus failed: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Results))
	}
	if !resp.Results[1].Cancelled {
		t.Error("expected result[1].Cancelled=true")
	}
}

func TestSearchNzbs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/nzb-search" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(NzbSearchResponse{
			Results: []NzbSearchResult{
				{NzbID: "nzb-1", Title: "Movie.2023.1080p"},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	resp, err := c.SearchNzbs(context.Background(), NzbSearchParams{Query: "Movie"})
	if err != nil {
		t.Fatalf("SearchNzbs failed: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resp.Results))
	}
	if resp.Results[0].NzbID != "nzb-1" {
		t.Errorf("nzb ID = %q, want nzb-1", resp.Results[0].NzbID)
	}
}

func TestDownloadNzb(t *testing.T) {
	nzbContent := []byte(`<?xml version="1.0"?><nzb><file>test</file></nzb>`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/nzb-download" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("nzbId") != "nzb-42" {
			t.Errorf("nzbId = %q, want nzb-42", r.URL.Query().Get("nzbId"))
		}
		w.Write(nzbContent)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	data, err := c.DownloadNzb(context.Background(), "nzb-42")
	if err != nil {
		t.Fatalf("DownloadNzb failed: %v", err)
	}
	if string(data) != string(nzbContent) {
		t.Errorf("nzb content mismatch")
	}
}

func TestDownloadNzbError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("NZB not found"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	_, err := c.DownloadNzb(context.Background(), "bad-id")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestGetUsenetCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/usenet-credentials" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(UsenetCredentials{
			Host:           "news.example.com",
			Port:           563,
			SSL:            true,
			Username:       "user1",
			Password:       "pass1",
			MaxConnections: 10,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	creds, err := c.GetUsenetCredentials(context.Background())
	if err != nil {
		t.Fatalf("GetUsenetCredentials failed: %v", err)
	}
	if creds.Host != "news.example.com" {
		t.Errorf("host = %q, want news.example.com", creds.Host)
	}
	if creds.Username != "user1" {
		t.Errorf("username = %q, want user1", creds.Username)
	}
	if creds.MaxConnections != 10 {
		t.Errorf("maxConnections = %d, want 10", creds.MaxConnections)
	}
}

func TestGetUsenetUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/usenet-usage" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(UsenetUsageResponse{
			UsedBytes:  5368709120,
			QuotaBytes: 10737418240,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	usage, err := c.GetUsenetUsage(context.Background())
	if err != nil {
		t.Fatalf("GetUsenetUsage failed: %v", err)
	}
	if usage.UsedBytes != 5368709120 {
		t.Errorf("usedBytes = %d", usage.UsedBytes)
	}
}

func TestConfigureDebrid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/debrid-config" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(ConfigureDebridResponse{Success: true})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	resp, err := c.ConfigureDebrid(context.Background(), ConfigureDebridRequest{
		Provider: "real-debrid",
		Token:    "rd-token-123",
	})
	if err != nil {
		t.Fatalf("ConfigureDebrid failed: %v", err)
	}
	if !resp.Success {
		t.Error("expected success=true")
	}
}

func TestBatchDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/batch-download" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(BatchDownloadResponse{
			Queued:   3,
			NotFound: 1,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	resp, err := c.BatchDownload(context.Background(), BatchDownloadRequest{})
	if err != nil {
		t.Fatalf("BatchDownload failed: %v", err)
	}
	if resp.Queued != 3 {
		t.Errorf("queued = %d, want 3", resp.Queued)
	}
}

func TestSyncLibrary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/library-sync" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(LibrarySyncResponse{
			Matched: 10,
			Synced:  15,
			Removed: 2,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	resp, err := c.SyncLibrary(context.Background(), LibrarySyncRequest{})
	if err != nil {
		t.Fatalf("SyncLibrary failed: %v", err)
	}
	if resp.Matched != 10 {
		t.Errorf("matched = %d, want 10", resp.Matched)
	}
	if resp.Synced != 15 {
		t.Errorf("synced = %d, want 15", resp.Synced)
	}
}

func TestHTMLErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	_, err := c.Register(context.Background(), RegisterRequest{AgentID: "x"})
	if err == nil {
		t.Fatal("expected error for HTML error page")
	}
}

func TestClient_ContextCancelled(t *testing.T) {
	// Servidor que bloquea hasta que el cliente se desconecta
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelar inmediatamente

	c := NewClient(srv.URL, "test-key", "unarr-test")
	_, err := c.Register(ctx, RegisterRequest{AgentID: "x"})
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
}

func TestClient_SlowServer_Timeout(t *testing.T) {
	// Servidor que tarda más que el timeout del cliente.
	// Usa time.Sleep para que el handler termine limpiamente cuando el server cierra.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond) // más largo que el timeout del cliente (50ms)
	}))
	defer srv.Close()

	// Crear cliente con timeout muy corto
	c := &Client{
		baseURL: srv.URL,
		apiKey:  "test-key",
		httpClient: &http.Client{
			Timeout: 50 * time.Millisecond,
		},
		userAgent: "unarr-test",
	}

	_, err := c.Register(context.Background(), RegisterRequest{AgentID: "timeout-test"})
	if err == nil {
		t.Fatal("expected timeout error from slow server")
	}
}

func TestClient_Sync_FullRequest(t *testing.T) {
	var received SyncRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/sync" {
			t.Errorf("path = %s, want /api/internal/agent/sync", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(SyncResponse{
			NewTasks: []Task{
				{ID: "task-from-server", InfoHash: "abc123def456abc123def456abc123def456abc1"},
			},
			Watching: true,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	resp, err := c.Sync(context.Background(), SyncRequest{
		AgentID:       "agent-sync-1",
		Version:       "0.6.0",
		OS:            "linux",
		Arch:          "amd64",
		FreeSlots:     2,
		DiskFreeBytes: 10 << 30, // 10 GB
	})
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	if len(resp.NewTasks) != 1 {
		t.Fatalf("expected 1 new task, got %d", len(resp.NewTasks))
	}
	if resp.NewTasks[0].ID != "task-from-server" {
		t.Errorf("task ID = %q, want task-from-server", resp.NewTasks[0].ID)
	}
	if !resp.Watching {
		t.Error("expected watching=true")
	}
	if received.AgentID != "agent-sync-1" {
		t.Errorf("received.AgentID = %q, want agent-sync-1", received.AgentID)
	}
	if received.FreeSlots != 2 {
		t.Errorf("received.FreeSlots = %d, want 2", received.FreeSlots)
	}
}

func TestClient_ReportWatchProgress(t *testing.T) {
	var received WatchProgressUpdate
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/watch-progress" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(WatchProgressResponse{Success: true})
	}))
	defer srv.Close()

	pct := 42
	c := NewClient(srv.URL, "test-key", "unarr-test")
	err := c.ReportWatchProgress(context.Background(), WatchProgressUpdate{
		TaskID:   "task-watch-001",
		Source:   "range",
		Progress: &pct,
	})
	if err != nil {
		t.Fatalf("ReportWatchProgress failed: %v", err)
	}
	if received.TaskID != "task-watch-001" {
		t.Errorf("taskID = %q, want task-watch-001", received.TaskID)
	}
	if received.Progress == nil || *received.Progress != 42 {
		t.Errorf("progress = %v, want 42", received.Progress)
	}
}

func TestClient_HTTPError_PlainText(t *testing.T) {
	// Error 500 con body plano (no JSON ni HTML largo)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	_, err := c.Register(context.Background(), RegisterRequest{AgentID: "x"})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *HTTPError (possibly wrapped), got %T: %v", err, err)
	}
	if httpErr.StatusCode != 500 {
		t.Errorf("StatusCode = %d, want 500", httpErr.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// WaitForWake tests
// ---------------------------------------------------------------------------

func TestWaitForWake_ReturnsTrue_OnWakeSignal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/wake" {
			t.Errorf("path = %s, want /api/internal/agent/wake", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(map[string]bool{"wake": true})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	woke, err := c.WaitForWake(context.Background())
	if err != nil {
		t.Fatalf("WaitForWake failed: %v", err)
	}
	if !woke {
		t.Error("expected wake=true")
	}
}

func TestWaitForWake_ReturnsFalse_OnTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server returns wake=false (long-poll timeout)
		json.NewEncoder(w).Encode(map[string]bool{"wake": false})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")
	woke, err := c.WaitForWake(context.Background())
	if err != nil {
		t.Fatalf("WaitForWake failed: %v", err)
	}
	if woke {
		t.Error("expected wake=false on server timeout")
	}
}

func TestWaitForWake_Error_OnUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid API key"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "bad-key", "unarr-test")
	_, err := c.WaitForWake(context.Background())
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
}

func TestWaitForWake_RespectsContextCancellation(t *testing.T) {
	// Server blocks until client disconnects
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	c := NewClient(srv.URL, "test-key", "unarr-test")
	_, err := c.WaitForWake(ctx)
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
}

func TestWaitForWake_SimulatesLongPoll(t *testing.T) {
	// Server holds connection briefly then responds with wake=true
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-ready:
		case <-r.Context().Done():
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"wake": true})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "unarr-test")

	resultCh := make(chan bool, 1)
	go func() {
		woke, err := c.WaitForWake(context.Background())
		if err != nil {
			t.Errorf("WaitForWake failed: %v", err)
		}
		resultCh <- woke
	}()

	// Simulate server waking after 50ms
	time.Sleep(50 * time.Millisecond)
	close(ready)

	select {
	case woke := <-resultCh:
		if !woke {
			t.Error("expected wake=true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForWake did not return in time")
	}
}
