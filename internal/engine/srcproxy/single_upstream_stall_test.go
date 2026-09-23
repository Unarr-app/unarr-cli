package srcproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// stallingUpstream answers the headers, serves the bytes below stallFrom and
// then sends nothing more (a hung CDN, a panel that holds the connection).
func stallingUpstream(t *testing.T, data []byte, stallFrom int64) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var first int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &first); err != nil {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)-int(first)))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		if first < stallFrom {
			_, _ = w.Write(data[first:stallFrom])
		}
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	return srv
}

func timedGet(url, rng string, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Range", rng)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return len(b), err
}

// A block the provider never delivers must not wedge the single link: once its
// requester gives up, the link is released within departedStall and the next
// reader is served. Before the watchdog it waited until the session closed.
func TestSingleUpstreamStalledBlockReleasesTheLink(t *testing.T) {
	data := payload(8 * blockSize)
	up := stallingUpstream(t, data, 4*blockSize)
	p := startProxy(t, Options{
		URL: up.URL + "/x.mkv", PinHead: blockSize, PinTail: blockSize,
		CacheBytes: 40 * blockSize, SingleUpstream: true,
	})

	if n, err := timedGet(p.ForegroundURL(), fmt.Sprintf("bytes=%d-%d", 4*blockSize, 5*blockSize-1), 300*time.Millisecond); err == nil && n == blockSize {
		t.Fatal("the stalled block was served; the upstream stub is broken")
	}
	start := time.Now()
	n, err := timedGet(p.ForegroundURL(), fmt.Sprintf("bytes=0-%d", blockSize-1), departedStall+3*time.Second)
	if err != nil || n != blockSize {
		t.Fatalf("healthy reader blocked behind the stalled link: n=%d err=%v after %s", n, err, time.Since(start))
	}
	if waited := time.Since(start); waited > departedStall+time.Second {
		t.Fatalf("healthy reader waited %s, want <= %s", waited, departedStall+time.Second)
	}
}

// A requester that keeps waiting on a hung block is itself released after the
// stall limit (no byte for StallTimeout), and so is everyone queued behind it.
func TestSingleUpstreamStallTimeoutWithRequesterStillWaiting(t *testing.T) {
	data := payload(8 * blockSize)
	up := stallingUpstream(t, data, 4*blockSize)
	p := startProxy(t, Options{
		URL: up.URL + "/x.mkv", PinHead: blockSize, PinTail: blockSize,
		CacheBytes: 40 * blockSize, SingleUpstream: true, StallTimeout: 500 * time.Millisecond,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = timedGet(p.ForegroundURL(), fmt.Sprintf("bytes=%d-%d", 4*blockSize, 5*blockSize-1), 10*time.Second)
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	n, err := timedGet(p.ForegroundURL(), fmt.Sprintf("bytes=0-%d", blockSize-1), 5*time.Second)
	if err != nil || n != blockSize {
		t.Fatalf("queued reader not served: n=%d err=%v", n, err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("queued reader waited %s behind a 500ms stall limit", waited)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled requester was never released")
	}
}

// A slow source that keeps sending is never cut by the watchdog: the limit is
// on silence, not on the time a block takes.
func TestSingleUpstreamSlowButMovingIsNotCut(t *testing.T) {
	data := payload(3 * blockSize)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var first int64
		_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &first)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)-int(first)))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		for off := first; off < int64(len(data)); off += 32 << 10 {
			end := min(off+32<<10, int64(len(data)))
			if _, err := w.Write(data[off:end]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(60 * time.Millisecond) // a block takes ~0.5 s, gaps 60 ms
		}
	}))
	t.Cleanup(srv.Close)
	p := startProxy(t, Options{
		URL: srv.URL + "/x.mkv", PinHead: blockSize, PinTail: blockSize,
		CacheBytes: 40 * blockSize, SingleUpstream: true, StallTimeout: 300 * time.Millisecond,
	})
	n, err := timedGet(p.ForegroundURL(), fmt.Sprintf("bytes=0-%d", 2*blockSize-1), 10*time.Second)
	if err != nil || n != 2*blockSize {
		t.Fatalf("slow-but-moving source was cut: n=%d err=%v", n, err)
	}
}

// Stalls, releases and Close leave no goroutine behind.
func TestSingleUpstreamStallNoGoroutineLeak(t *testing.T) {
	data := payload(8 * blockSize)
	up := stallingUpstream(t, data, 4*blockSize)
	before := runtime.NumGoroutine()
	p, err := Start(Options{
		URL: up.URL + "/x.mkv", Dir: t.TempDir(), PinHead: blockSize, PinTail: blockSize,
		CacheBytes: 40 * blockSize, SingleUpstream: true, StallTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, _ = timedGet(p.ForegroundURL(), fmt.Sprintf("bytes=%d-%d", 4*blockSize, 5*blockSize-1), 200*time.Millisecond)
		_, _ = timedGet(p.ForegroundURL(), fmt.Sprintf("bytes=0-%d", blockSize-1), 3*time.Second)
	}
	closed := make(chan struct{})
	go func() { _ = p.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close deadlocked")
	}
	http.DefaultClient.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines %d -> %d after Close", before, after)
	}
}
