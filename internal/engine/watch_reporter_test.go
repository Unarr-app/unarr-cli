package engine

import (
	"context"
	"net/http"
	"os"
	"testing"
)

// ---------------------------------------------------------------------------
// parseRangeStart
// ---------------------------------------------------------------------------

func TestParseRangeStart(t *testing.T) {
	tests := []struct {
		header string
		want   int64
	}{
		{"bytes=0-", 0},
		{"bytes=1024-", 1024},
		{"bytes=5000-9999", 5000},
		{"bytes=1048576-", 1048576},
		{"", -1},
		{"invalid", -1},
		{"bytes=", -1},
		{"bytes=-500", -1},
	}

	for _, tc := range tests {
		got := parseRangeStart(tc.header)
		if got != tc.want {
			t.Errorf("parseRangeStart(%q) = %d, want %d", tc.header, got, tc.want)
		}
	}
}

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

	pos, dur := ss.EstimatedProgress()
	if pos != 50 || dur != 100 {
		t.Errorf("expected (50, 100), got (%d, %d)", pos, dur)
	}
}

func TestEstimatedProgress_CapsAt100(t *testing.T) {
	ss := &StreamServer{}
	ss.totalFileSize.Store(1000)
	ss.maxByteOffset.Store(1500)

	pos, dur := ss.EstimatedProgress()
	if pos != 100 || dur != 100 {
		t.Errorf("expected (100, 100), got (%d, %d)", pos, dur)
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

func TestStreamServerRangeTracking(t *testing.T) {
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

	// 1. Non-range GET — maxByteOffset stays 0
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if srv.maxByteOffset.Load() != 0 {
		t.Errorf("non-range: expected 0, got %d", srv.maxByteOffset.Load())
	}

	// 2. Range: bytes=5000- → offset 5000
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Range", "bytes=5000-")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Range GET: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("expected 206, got %d", resp.StatusCode)
	}
	if srv.maxByteOffset.Load() != 5000 {
		t.Errorf("expected 5000, got %d", srv.maxByteOffset.Load())
	}

	// 3. Higher offset
	req, _ = http.NewRequest("GET", url, nil)
	req.Header.Set("Range", "bytes=8000-")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Range GET 2: %v", err)
	}
	resp.Body.Close()

	if srv.maxByteOffset.Load() != 8000 {
		t.Errorf("expected 8000, got %d", srv.maxByteOffset.Load())
	}

	// 4. Lower offset does NOT regress
	req, _ = http.NewRequest("GET", url, nil)
	req.Header.Set("Range", "bytes=2000-")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Range GET 3: %v", err)
	}
	resp.Body.Close()

	if srv.maxByteOffset.Load() != 8000 {
		t.Errorf("expected still 8000, got %d", srv.maxByteOffset.Load())
	}

	// 5. Verify progress estimate
	pos, dur := srv.EstimatedProgress()
	// 8000/10240 = 78.1% → 78
	if pos < 78 || pos > 79 {
		t.Errorf("expected pos ~78, got %d", pos)
	}
	if dur != 100 {
		t.Errorf("expected dur=100, got %d", dur)
	}
}
