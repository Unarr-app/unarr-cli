package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torrentclaw/torrentclaw-cli/internal/agent"
)

func TestDebridAvailable(t *testing.T) {
	d := NewDebridDownloader()

	t.Run("available when DirectURL is set", func(t *testing.T) {
		task := &Task{DirectURL: "https://cdn.example.com/file.mkv"}
		ok, err := d.Available(context.Background(), task)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ok {
			t.Error("should be available when DirectURL is set")
		}
	})

	t.Run("not available when DirectURL is empty", func(t *testing.T) {
		task := &Task{DirectURL: ""}
		ok, err := d.Available(context.Background(), task)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Error("should not be available when DirectURL is empty")
		}
	})
}

func TestDebridDownloadSuccess(t *testing.T) {
	fileContent := strings.Repeat("x", 1024*100) // 100KB file

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(fileContent)))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fileContent))
	}))
	defer srv.Close()

	d := NewDebridDownloader()
	outputDir := t.TempDir()

	task := &Task{
		ID:             "debrid-test-001",
		InfoHash:       "abc123def456abc123def456abc123def456abc1",
		Title:          "Test Movie",
		DirectURL:      srv.URL + "/file.mkv",
		DirectFileName: "Test.Movie.2026.1080p.mkv",
		Status:         StatusDownloading,
	}

	progressCh := make(chan Progress, 100)
	result, err := d.Download(context.Background(), task, outputDir, progressCh)
	close(progressCh)

	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}

	if result.Method != MethodDebrid {
		t.Errorf("Method = %q, want debrid", result.Method)
	}
	if result.FileName != "Test.Movie.2026.1080p.mkv" {
		t.Errorf("FileName = %q, want Test.Movie.2026.1080p.mkv", result.FileName)
	}
	if result.Size != int64(len(fileContent)) {
		t.Errorf("Size = %d, want %d", result.Size, len(fileContent))
	}

	// Verify file exists on disk
	data, err := os.ReadFile(result.FilePath)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if len(data) != len(fileContent) {
		t.Errorf("file size = %d, want %d", len(data), len(fileContent))
	}

	// Verify task progress was updated
	if task.DownloadedBytes != int64(len(fileContent)) {
		t.Errorf("task.DownloadedBytes = %d, want %d", task.DownloadedBytes, len(fileContent))
	}
}

func TestDebridDownloadNoURL(t *testing.T) {
	d := NewDebridDownloader()
	task := &Task{ID: "no-url-001", DirectURL: ""}
	progressCh := make(chan Progress, 10)

	_, err := d.Download(context.Background(), task, t.TempDir(), progressCh)
	if err == nil {
		t.Error("expected error for empty DirectURL")
	}
	if !strings.Contains(err.Error(), "no direct URL") {
		t.Errorf("error = %q, should mention no direct URL", err.Error())
	}
}

func TestDebridDownloadHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	d := NewDebridDownloader()
	task := &Task{
		ID:             "http-err-001",
		DirectURL:      srv.URL + "/expired",
		DirectFileName: "expired.mkv",
	}
	progressCh := make(chan Progress, 10)

	_, err := d.Download(context.Background(), task, t.TempDir(), progressCh)
	if err == nil {
		t.Error("expected error for HTTP 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %q, should contain 403", err.Error())
	}
}

func TestDebridDownloadExpiredURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone) // 410 — URL expired
	}))
	defer srv.Close()

	d := NewDebridDownloader()
	task := &Task{
		ID:             "expired-001",
		DirectURL:      srv.URL + "/expired",
		DirectFileName: "expired.mkv",
	}
	progressCh := make(chan Progress, 10)

	_, err := d.Download(context.Background(), task, t.TempDir(), progressCh)
	if err == nil {
		t.Error("expected error for HTTP 410 (expired URL)")
	}
	if !strings.Contains(err.Error(), "410") {
		t.Errorf("error = %q, should contain 410", err.Error())
	}
}

func TestDebridDownloadUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	d := NewDebridDownloader()
	task := &Task{
		ID:             "unauth-001",
		DirectURL:      srv.URL + "/unauth",
		DirectFileName: "unauth.mkv",
	}
	progressCh := make(chan Progress, 10)

	_, err := d.Download(context.Background(), task, t.TempDir(), progressCh)
	if err == nil {
		t.Error("expected error for HTTP 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %q, should contain 401", err.Error())
	}
}

func TestDebridDownloadResume(t *testing.T) {
	fullContent := "HEADER_ALREADY_DOWNLOADED_REST_OF_FILE"
	alreadyDownloaded := "HEADER_ALREADY_DOWNLOADED_"
	remaining := "REST_OF_FILE"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "" {
			// Parse "bytes=26-"
			var start int64
			fmt.Sscanf(rangeHeader, "bytes=%d-", &start)
			if start == int64(len(alreadyDownloaded)) {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(remaining)))
				w.WriteHeader(http.StatusPartialContent)
				w.Write([]byte(remaining))
				return
			}
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(fullContent)))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fullContent))
	}))
	defer srv.Close()

	d := NewDebridDownloader()
	outputDir := t.TempDir()
	fileName := "resume-test.mkv"

	// Create partial file
	partialPath := filepath.Join(outputDir, fileName)
	if err := os.WriteFile(partialPath, []byte(alreadyDownloaded), 0o644); err != nil {
		t.Fatalf("write partial file: %v", err)
	}

	task := &Task{
		ID:             "resume-001",
		DirectURL:      srv.URL + "/file.mkv",
		DirectFileName: fileName,
		Status:         StatusDownloading,
	}

	progressCh := make(chan Progress, 100)
	result, err := d.Download(context.Background(), task, outputDir, progressCh)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}

	// Verify total size includes both parts
	if result.Size != int64(len(fullContent)) {
		t.Errorf("Size = %d, want %d", result.Size, len(fullContent))
	}

	// Verify file content
	data, err := os.ReadFile(result.FilePath)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(data) != fullContent {
		t.Errorf("file content = %q, want %q", string(data), fullContent)
	}
}

func TestDebridDownloadCancel(t *testing.T) {
	// Server that sends a chunk then waits
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(http.StatusOK)
		// Write some data so the download starts
		w.Write([]byte(strings.Repeat("x", 4096)))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		// Block until client disconnects
		<-r.Context().Done()
	}))
	defer srv.Close()

	d := NewDebridDownloader()
	task := &Task{
		ID:             "cancel-001",
		DirectURL:      srv.URL + "/slow",
		DirectFileName: "slow.mkv",
		Status:         StatusDownloading,
	}

	progressCh := make(chan Progress, 100)

	errCh := make(chan error, 1)
	go func() {
		_, err := d.Download(context.Background(), task, t.TempDir(), progressCh)
		errCh <- err
	}()

	// Wait for server to confirm download started, then cancel
	<-started
	d.Cancel("cancel-001")

	err := <-errCh
	if err == nil {
		t.Error("expected error after cancel")
	}
}

func TestDebridDownloadPause(t *testing.T) {
	// Server that sends a chunk then waits
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(http.StatusOK)
		// Write enough data to create file
		w.Write([]byte(strings.Repeat("x", 8192)))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		// Block until client disconnects
		<-r.Context().Done()
	}))
	defer srv.Close()

	d := NewDebridDownloader()
	outputDir := t.TempDir()
	task := &Task{
		ID:             "pause-001",
		DirectURL:      srv.URL + "/slow",
		DirectFileName: "pauseable.mkv",
		Status:         StatusDownloading,
	}

	progressCh := make(chan Progress, 100)
	errCh := make(chan error, 1)
	go func() {
		_, err := d.Download(context.Background(), task, outputDir, progressCh)
		errCh <- err
	}()

	// Wait for server to confirm data was sent, then pause
	<-started
	time.Sleep(50 * time.Millisecond) // small delay for file write
	d.Pause("pause-001")

	<-errCh

	// Verify partial file exists on disk (pause keeps files)
	partialPath := filepath.Join(outputDir, "pauseable.mkv")
	fi, err := os.Stat(partialPath)
	if err != nil {
		t.Fatalf("partial file should exist after pause: %v", err)
	}
	if fi.Size() == 0 {
		t.Error("partial file should have some bytes")
	}
}

func TestDebridDownloadFallbackFilename(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "5")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "hello")
	}))
	defer srv.Close()

	d := NewDebridDownloader()

	t.Run("uses Title when DirectFileName is empty", func(t *testing.T) {
		task := &Task{
			ID:        "fallback-001",
			Title:     "My Movie Title",
			DirectURL: srv.URL + "/file",
			Status:    StatusDownloading,
		}
		progressCh := make(chan Progress, 10)
		result, err := d.Download(context.Background(), task, t.TempDir(), progressCh)
		if err != nil {
			t.Fatalf("Download failed: %v", err)
		}
		if result.FileName != "My Movie Title" {
			t.Errorf("FileName = %q, want 'My Movie Title'", result.FileName)
		}
	})

	t.Run("uses InfoHash when both are empty", func(t *testing.T) {
		task := &Task{
			ID:        "fallback-002",
			InfoHash:  "abc123",
			DirectURL: srv.URL + "/file",
			Status:    StatusDownloading,
		}
		progressCh := make(chan Progress, 10)
		result, err := d.Download(context.Background(), task, t.TempDir(), progressCh)
		if err != nil {
			t.Fatalf("Download failed: %v", err)
		}
		if result.FileName != "abc123" {
			t.Errorf("FileName = %q, want 'abc123'", result.FileName)
		}
	})
}

func TestDebridShutdown(t *testing.T) {
	d := NewDebridDownloader()
	err := d.Shutdown(context.Background())
	if err != nil {
		t.Errorf("Shutdown should not error: %v", err)
	}
}

func TestNewTaskFromAgentWithDirectURL(t *testing.T) {
	at := agent.Task{
		ID:              "uuid-debrid",
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "Debrid Movie",
		PreferredMethod: "debrid",
		DirectURL:       "https://cdn.torbox.app/dl/abc123/movie.mkv",
		DirectFileName:  "Movie.2026.1080p.mkv",
	}

	task := NewTaskFromAgent(at)

	if task.DirectURL != "https://cdn.torbox.app/dl/abc123/movie.mkv" {
		t.Errorf("DirectURL = %q", task.DirectURL)
	}
	if task.DirectFileName != "Movie.2026.1080p.mkv" {
		t.Errorf("DirectFileName = %q", task.DirectFileName)
	}
	if task.PreferredMethod != "debrid" {
		t.Errorf("PreferredMethod = %q", task.PreferredMethod)
	}
}

func TestDebridMethod(t *testing.T) {
	d := NewDebridDownloader()
	if d.Method() != MethodDebrid {
		t.Errorf("Method = %q, want debrid", d.Method())
	}
}
