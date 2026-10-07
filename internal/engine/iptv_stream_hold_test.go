package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// countingHandler tracks how many provider connections are open at once.
type countingHandler struct {
	next    http.Handler
	active  atomic.Int32
	maxSeen atomic.Int32
}

func (c *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := c.active.Add(1)
	defer c.active.Add(-1)
	for {
		m := c.maxSeen.Load()
		if n <= m || c.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	c.next.ServeHTTP(w, r)
}

func waitActive(t *testing.T, c *countingHandler, want int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for c.active.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("open provider connections = %d, want %d", c.active.Load(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// I1: a stream about to open the IPTV account waits for the running download to
// let go of its connection, keeps downloads parked while it plays even when the
// browser's lease is gone, and gives the account back when it ends.
func TestHoldForStreamParksTheDownloadIndependentlyOfTheLease(t *testing.T) {
	srv := &iptvServer{body: iptvBody(), half: 700 * 1024, stallFirst: true, stalled: make(chan struct{})}
	counter := &countingHandler{next: srv}
	ts := httptest.NewServer(counter)
	defer ts.Close()
	hold := NewPlaybackHold(time.Minute)
	d := NewIptvDownloader(hold)
	outputDir := t.TempDir()
	task := iptvTask("streamhold", ts.URL+"/movie/u/p/9.mkv")
	done := make(chan error, 1)
	var res *Result
	go func() {
		r, err := d.Download(context.Background(), task, outputDir, make(chan Progress, 100))
		res = r
		done <- err
	}()
	<-srv.stalled
	dest, _ := safePath(outputDir, debridFileName(task))
	waitPartialSize(t, partialPath(dest), int64(srv.half))
	waitActive(t, counter, 1)

	// The stream starts: HoldForStream must not return while the transfer still
	// has the provider connection (the attempt holds conn until it unwinds).
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	release := d.HoldForStream(ctx)
	if ctx.Err() != nil {
		t.Fatal("HoldForStream waited out its whole budget: the transfer never let go")
	}
	select {
	case d.conn <- struct{}{}:
		<-d.conn
	default:
		t.Fatal("HoldForStream returned while the transfer still held the provider connection")
	}
	waitActive(t, counter, 0)

	// The browser lease is released (tab closed, or its release beat the
	// teardown): the stream still runs, so nothing may reconnect.
	hold.Set(false)
	time.Sleep(200 * time.Millisecond)
	if n := srv.requests.Load(); n != 1 {
		t.Fatalf("requests while the stream is live = %d, want 1", n)
	}

	release()
	release() // idempotent: a second release must not unbalance the pin count
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Download: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download did not resume after the stream released the account")
	}
	data, _ := os.ReadFile(res.FilePath)
	if string(data) != srv.body {
		t.Fatal("resumed file differs from the source")
	}
	if m := counter.maxSeen.Load(); m != 1 {
		t.Fatalf("at most one provider connection at a time, saw %d", m)
	}
	if hold.Held() {
		t.Fatal("hold still in force after the only pin was released and the lease cleared")
	}
}

// With no download running, HoldForStream returns at once.
func TestHoldForStreamIdleReturnsImmediately(t *testing.T) {
	d := NewIptvDownloader(NewPlaybackHold(time.Minute))
	began := time.Now()
	release := d.HoldForStream(context.Background())
	if time.Since(began) > 100*time.Millisecond {
		t.Fatalf("idle HoldForStream took %s", time.Since(began))
	}
	release()
}
