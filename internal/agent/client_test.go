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
