package engine

// Dead-article invariants for the /usenet endpoint (phase 1: fail fast, fail
// once, fail cleanly). Before, one article the server answers 430 for cost ~1 s
// and 7 BODY commands per request that touched it (3 reader attempts, each
// reconnecting and re-asking), 20 BODY for a sequential read crossing it, the
// same again on every later request, and the client got a 206 whose body just
// stopped.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const deadSeg = 5

// TestUsenetRangeOnDeadArticleFailsCleanlyAndFast: a range whose first byte sits
// in a dead article gets an error status and no video body, costs exactly one
// BODY for that article, and a second request into it costs zero and returns at
// once.
func TestUsenetRangeOnDeadArticleFailsCleanlyAndFast(t *testing.T) {
	f := buildReuseFixture(t, 12, 0, deadSeg)
	deadID := f.ids[deadSeg]
	f.cf.settle(t)
	lo := deadSeg*reusePartSize + 100

	request := func() (int, string, int, time.Duration) {
		start := time.Now()
		resp := f.rangeRequest(t, lo, lo+999)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Content-Type"), len(body), time.Since(start)
	}

	code, ct, n, cold := request()
	f.cf.settle(t)
	t.Logf("cold request into the dead article: status %d (%s, %d body bytes) in %s, %d BODY for it",
		code, ct, n, cold.Round(time.Millisecond), f.cf.count(deadID))
	if code == http.StatusPartialContent || code < 500 {
		t.Fatalf("status %d, want a 5xx error status (no 206 with a cut body)", code)
	}
	if strings.HasPrefix(ct, "video/") {
		t.Fatalf("error response carries Content-Type %q", ct)
	}
	if got := f.cf.count(deadID); got != 1 {
		t.Fatalf("dead article fetched %d times by one request, want exactly 1", got)
	}

	code, _, _, warm := request()
	f.cf.settle(t)
	t.Logf("warm request into the dead article: status %d in %s, %d BODY for it in total",
		code, warm.Round(time.Millisecond), f.cf.count(deadID))
	if code == http.StatusPartialContent || code < 500 {
		t.Fatalf("second request status %d, want a 5xx", code)
	}
	if got := f.cf.count(deadID); got != 1 {
		t.Fatalf("second request fetched the dead article again (%d BODY in total), want 0 more", got)
	}
	if warm > 50*time.Millisecond {
		t.Fatalf("second request took %s, want < 50ms (memoised verdict)", warm)
	}
}

// TestUsenetMidRangeDeadArticleAbortsOnce: when the dead article is reached after
// the body started, the response is aborted (a transport error for the client,
// never a short body that looks complete), carries only correct bytes, and is
// logged exactly once.
func TestUsenetMidRangeDeadArticleAbortsOnce(t *testing.T) {
	f := buildReuseFixture(t, 12, 0, deadSeg)

	var logs bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&logs)
	restore := sync.OnceFunc(func() { log.SetOutput(orig) })
	defer restore()

	lo, hi := (deadSeg-2)*reusePartSize, (deadSeg+2)*reusePartSize-1
	resp := f.rangeRequest(t, lo, hi)
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	restore()

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status %d, want 206 (the range starts on a healthy article)", resp.StatusCode)
	}
	if err == nil {
		t.Fatalf("body ended after %d of %d bytes with no transport error: a silent short body", len(got), hi-lo+1)
	}
	if len(got) > 2*reusePartSize || !bytes.Equal(got, f.content[lo:lo+len(got)]) {
		t.Fatalf("served %d bytes; want only correct bytes from before the dead article", len(got))
	}
	if n := strings.Count(logs.String(), "aborting /usenet/"); n != 1 {
		t.Fatalf("abort logged %d times, want exactly 1\n%s", n, logs.String())
	}
}

// TestUsenetReadaheadAcrossDeadArticleFetchesItOnce: a sequential read with the
// default read-ahead that runs into a dead article issues at most one BODY for
// it — read-ahead and the foreground read must not each retry it.
func TestUsenetReadaheadAcrossDeadArticleFetchesItOnce(t *testing.T) {
	f := buildReuseFixture(t, 12, 0, deadSeg)
	rd := f.handle.Provider.NewFileReader(context.Background())
	n, err := io.Copy(io.Discard, rd)
	if err == nil {
		t.Fatal("sequential read crossed a dead article without an error")
	}
	f.cf.settle(t)
	_ = rd.Close()
	got := f.cf.count(f.ids[deadSeg])
	t.Logf("sequential read stopped after %d bytes; %d BODY for the dead article, %d BODY in total",
		n, got, f.cf.total())
	if n != int64(deadSeg*reusePartSize) {
		t.Fatalf("read %d bytes before failing, want %d (everything before the dead article)", n, deadSeg*reusePartSize)
	}
	if got > 1 {
		t.Fatalf("dead article fetched %d times, want <= 1", got)
	}
}

// TestUsenetTransportTimeoutIsNotAnArticleVerdict: a connection-level timeout
// (here a reconnect whose dial timed out) says nothing about the article. It must
// be retried, never memoised, and answered as a plain 502 — not a 504 stall that
// names the article as missing.
func TestUsenetTransportTimeoutIsNotAnArticleVerdict(t *testing.T) {
	f := buildReuseFixture(t, 12, 0, -1)
	id := f.ids[deadSeg]
	dialTimeout := &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
	f.cf.mu.Lock()
	f.cf.errs = map[string]error{id: fmt.Errorf("nntp: body failed and reconnect failed: %w", dialTimeout)}
	f.cf.mu.Unlock()

	lo := deadSeg*reusePartSize + 100
	for i := 0; i < 2; i++ {
		resp := f.rangeRequest(t, lo, lo+999)
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("request %d: status %d, want 502 (transport failure, not a stall)", i+1, resp.StatusCode)
		}
		if h := resp.Header.Get("X-Unarr-Missing-Article"); h != "" {
			t.Fatalf("request %d: X-Unarr-Missing-Article = %q for a transport failure", i+1, h)
		}
	}
	f.cf.settle(t)
	if got := f.cf.count(id); got < 4 {
		t.Fatalf("article fetched %d times over 2 requests; a transport error must be retried and not memoised", got)
	}
}

// TestUsenetConcurrentReadersOfDeadArticleShareOneBody: readers hitting the same
// dead article at the same time share one BODY and one verdict.
func TestUsenetConcurrentReadersOfDeadArticleShareOneBody(t *testing.T) {
	f := buildReuseFixture(t, 12, 50*time.Millisecond, deadSeg)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rd := f.handle.Provider.NewFileReader(context.Background())
			defer rd.Close()
			if _, err := rd.Seek(int64(deadSeg*reusePartSize+7), io.SeekStart); err != nil {
				t.Errorf("seek: %v", err)
				return
			}
			if _, err := rd.Read(make([]byte, 32)); err == nil {
				t.Error("read of a dead article succeeded")
			}
		}()
	}
	wg.Wait()
	if got := f.cf.count(f.ids[deadSeg]); got != 1 {
		t.Fatalf("6 concurrent readers issued %d BODY for the dead article, want 1", got)
	}
}
