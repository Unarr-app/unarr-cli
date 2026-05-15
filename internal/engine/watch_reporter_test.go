package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torrentclaw/unarr/internal/agent"
)

// ---------------------------------------------------------------------------
// StreamServer.EstimatedProgress
// ---------------------------------------------------------------------------

func TestEstimatedProgress_NoFile(t *testing.T) {
	ss := &StreamServer{}
	pos, dur := ss.EstimatedProgress()
	if pos != 0 || dur != 0 {
		t.Errorf("expected (0, 0), got (%d, %d)", pos, dur)
	}
}

func TestEstimatedProgress_HalfWay(t *testing.T) {
	ss := &StreamServer{}
	ss.totalFileSize.Store(1000)
	ss.maxByteOffset.Store(500)

	pos, _ := ss.EstimatedProgress()
	if pos != 50 {
		t.Errorf("expected pct=50, got %d", pos)
	}
}

func TestEstimatedProgress_CapsAt100(t *testing.T) {
	ss := &StreamServer{}
	ss.totalFileSize.Store(1000)
	ss.maxByteOffset.Store(1500)

	pos, _ := ss.EstimatedProgress()
	if pos != 100 {
		t.Errorf("expected pct=100, got %d", pos)
	}
}

// ---------------------------------------------------------------------------
// maxByteOffset only increases (simulated Range tracking)
// ---------------------------------------------------------------------------

func TestMaxByteOffsetNeverRegresses(t *testing.T) {
	ss := &StreamServer{}
	ss.totalFileSize.Store(10000)

	offsets := []int64{0, 2000, 5000, 3000, 8000, 4000}
	for _, off := range offsets {
		for {
			cur := ss.maxByteOffset.Load()
			if off <= cur || ss.maxByteOffset.CompareAndSwap(cur, off) {
				break
			}
		}
	}

	if ss.maxByteOffset.Load() != 8000 {
		t.Errorf("expected 8000, got %d", ss.maxByteOffset.Load())
	}
}

// ---------------------------------------------------------------------------
// End-to-end: real HTTP server with Range requests
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// WatchReporter.sendReport via the agent API
// ---------------------------------------------------------------------------

func TestWatchReporter_NewWatchReporter(t *testing.T) {
	c := agent.NewClient("http://localhost", "", "test")
	ss := &StreamServer{}
	wr := NewWatchReporter(c, ss, "task-1")
	if wr.taskID != "task-1" || wr.client != c || wr.server != ss {
		t.Errorf("NewWatchReporter fields not wired: %+v", wr)
	}
}

func TestWatchReporter_sendReportSkipsZeroProgress(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	ss := &StreamServer{}
	// totalFileSize == 0 → EstimatedProgress returns (0, 0) → sendReport skips.
	c := agent.NewClient(srv.URL, "", "test")
	wr := NewWatchReporter(c, ss, "task-1")
	wr.sendReport(context.Background())
	if hits.Load() != 0 {
		t.Errorf("expected no API calls when progress=0, got %d", hits.Load())
	}
}

func TestWatchReporter_sendReportPostsProgress(t *testing.T) {
	var captured atomic.Pointer[agent.WatchProgressUpdate]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var update agent.WatchProgressUpdate
		_ = json.NewDecoder(r.Body).Decode(&update)
		captured.Store(&update)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ss := &StreamServer{}
	ss.totalFileSize.Store(1000)
	ss.maxByteOffset.Store(250) // 25%
	ss.durationSec.Store(120)

	c := agent.NewClient(srv.URL, "", "test")
	wr := NewWatchReporter(c, ss, "task-12345678")
	wr.sendReport(context.Background())

	got := captured.Load()
	if got == nil {
		t.Fatal("expected a watch-progress POST")
	}
	if got.TaskID != "task-12345678" {
		t.Errorf("TaskID = %q", got.TaskID)
	}
	if got.Progress == nil || *got.Progress != 25 {
		t.Errorf("Progress = %v, want 25", got.Progress)
	}
	if got.Duration == nil || *got.Duration != 120 {
		t.Errorf("Duration = %v, want 120", got.Duration)
	}
	if got.Position == nil || *got.Position != 30 {
		t.Errorf("Position = %v, want 30", got.Position)
	}

	// Repeat report at same percentage — should NOT POST again.
	captured.Store(nil)
	wr.sendReport(context.Background())
	if captured.Load() != nil {
		t.Errorf("repeat sendReport at same pct should be a no-op")
	}
}

func TestWatchReporter_RunStopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ss := &StreamServer{}
	c := agent.NewClient(srv.URL, "", "test")
	wr := NewWatchReporter(c, ss, "task-x")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		wr.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestStreamServerByteTracking(t *testing.T) {
	// Create temp file (10 KB)
	tmpFile := t.TempDir() + "/test.mp4"
	data := make([]byte, 10240)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		t.Fatal(err)
	}

	srv := NewStreamServer(0) // UPnP off by default — keep test hermetic
	ctx := context.Background()
	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer srv.Shutdown(ctx)
	srv.SetFile(NewDiskFileProvider(tmpFile), "test-task")
	url := srv.URL()

	// 1. Full GET — reads all bytes, maxByteOffset reaches file size
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if srv.maxByteOffset.Load() != 10240 {
		t.Errorf("full read: expected 10240, got %d", srv.maxByteOffset.Load())
	}

	// 2. Reset and verify progress after partial read via Range
	srv.SetFile(NewDiskFileProvider(tmpFile), "test-task-2")
	if srv.maxByteOffset.Load() != 0 {
		t.Errorf("after reset: expected 0, got %d", srv.maxByteOffset.Load())
	}

	// Range request reads from offset 5000 to end (5240 bytes)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Range", "bytes=5000-")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Range GET: %v", err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("expected 206, got %d", resp.StatusCode)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// The reader reads 5240 bytes (from offset 5000 to 10240).
	// maxByteOffset tracks the read position, which ends at 10240.
	got := srv.maxByteOffset.Load()
	if got != 10240 {
		t.Errorf("after range read: expected 10240, got %d", got)
	}

	// 3. Verify progress reaches 100%
	pos, _ := srv.EstimatedProgress()
	if pos != 100 {
		t.Errorf("expected pct=100, got %d", pos)
	}
}
