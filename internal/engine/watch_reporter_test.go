package engine

import (
	"context"
	"io"
	"net/http"
	"os"
	"testing"
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

	srv := NewStreamServer(0)
	srv.disableUPnP = true
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
