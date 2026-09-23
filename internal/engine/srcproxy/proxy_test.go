package srcproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// payload is deterministic but non-periodic at block granularity, so a block
// served from the wrong slot/offset is always detected.
func payload(n int) []byte {
	b := make([]byte, n)
	x := uint32(2463534242)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return b
}

type upstream struct {
	data     []byte
	requests atomic.Int64
	sent     atomic.Int64
	// dropAfter>0 cuts the FIRST response after that many bytes (simulated CDN reset).
	dropAfter atomic.Int64
	// deny makes every request for the path "/old" answer 403.
	srv *httptest.Server
}

func newUpstream(t *testing.T, data []byte) *upstream {
	t.Helper()
	u := &upstream{data: data}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.requests.Add(1)
		if r.URL.Path == "/old" {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		var first int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &first); err != nil || first >= int64(len(data)) {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		body := data[first:]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, len(data)-1, len(data)))
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		if cut := u.dropAfter.Swap(0); cut > 0 && cut < int64(len(body)) {
			n, _ := w.Write(body[:cut])
			u.sent.Add(int64(n))
			panic(http.ErrAbortHandler) // abrupt close mid-body
		}
		n, _ := w.Write(body)
		u.sent.Add(int64(n))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func startProxy(t *testing.T, opts Options) *Proxy {
	t.Helper()
	opts.Dir = t.TempDir()
	p, err := Start(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func fetchRange(t *testing.T, url, rng string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

func TestProxyServesExactBytes(t *testing.T) {
	data := payload(5*blockSize + 1234)
	u := newUpstream(t, data)
	p := startProxy(t, Options{URL: u.srv.URL + "/media.mkv", PinHead: blockSize, PinTail: blockSize})

	cases := []struct {
		rng         string
		first, last int
	}{
		{"bytes=0-99", 0, 99},
		{"bytes=100-", 100, len(data) - 1},
		{fmt.Sprintf("bytes=%d-%d", blockSize-10, 2*blockSize+10), blockSize - 10, 2*blockSize + 10},
		{fmt.Sprintf("bytes=%d-", 5*blockSize+1000), 5*blockSize + 1000, len(data) - 1},
		{fmt.Sprintf("bytes=%d-%d", len(data)-5, len(data)+500), len(data) - 5, len(data) - 1},
	}
	for _, c := range cases {
		code, got := fetchRange(t, p.ForegroundURL(), c.rng)
		if code != http.StatusPartialContent || !bytes.Equal(got, data[c.first:c.last+1]) {
			t.Fatalf("%s: code=%d len=%d want len=%d", c.rng, code, len(got), c.last-c.first+1)
		}
	}
	if code, got := fetchRange(t, p.BackgroundURL(), ""); code != http.StatusOK || !bytes.Equal(got, data) {
		t.Fatalf("full body: code=%d len=%d", code, len(got))
	}
	if code, _ := fetchRange(t, p.ForegroundURL(), fmt.Sprintf("bytes=%d-", len(data))); code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("past EOF: code=%d", code)
	}
	if code, _ := fetchRange(t, p.base()+"fg/other.mkv", "bytes=0-"); code != http.StatusNotFound {
		t.Fatalf("unknown path: code=%d", code)
	}
}

// The regression this package exists for: a second reader of the container
// header and seek index must not touch the network at all.
func TestProxyHeaderAndIndexAreFreeOnReread(t *testing.T) {
	data := payload(40 * blockSize)
	u := newUpstream(t, data)
	p := startProxy(t, Options{URL: u.srv.URL + "/m.mkv", PinHead: 4 * blockSize, PinTail: 2 * blockSize})

	head := fmt.Sprintf("bytes=0-%d", 4*blockSize-1)
	tail := fmt.Sprintf("bytes=%d-", 38*blockSize)
	fetchRange(t, p.ForegroundURL(), head)
	fetchRange(t, p.ForegroundURL(), tail)
	before := p.Stats()

	// Churn the LRU well past capacity; pinned blocks must survive it.
	small := startProxy(t, Options{URL: u.srv.URL + "/m.mkv", PinHead: 4 * blockSize, PinTail: 2 * blockSize, CacheBytes: 1})
	fetchRange(t, small.ForegroundURL(), head)
	fetchRange(t, small.ForegroundURL(), tail)
	fetchRange(t, small.ForegroundURL(), fmt.Sprintf("bytes=%d-%d", 4*blockSize, 38*blockSize-1))
	smallBefore := small.Stats()

	for _, px := range []*Proxy{p, small} {
		if _, got := fetchRange(t, px.ForegroundURL(), head); !bytes.Equal(got, data[:4*blockSize]) {
			t.Fatal("head reread corrupted")
		}
		if _, got := fetchRange(t, px.ForegroundURL(), tail); !bytes.Equal(got, data[38*blockSize:]) {
			t.Fatal("tail reread corrupted")
		}
	}
	if after := p.Stats(); after.UpstreamBytes != before.UpstreamBytes || after.UpstreamRequests != before.UpstreamRequests {
		t.Fatalf("reread hit the network: %+v -> %+v", before, after)
	}
	if after := small.Stats(); after.UpstreamBytes != smallBefore.UpstreamBytes {
		t.Fatalf("pinned blocks were evicted: %+v -> %+v", smallBefore, after)
	}
}

func TestProxyLRUEvictionKeepsDataCorrect(t *testing.T) {
	data := payload(200 * blockSize)
	u := newUpstream(t, data)
	p := startProxy(t, Options{URL: u.srv.URL + "/m.mkv", PinHead: blockSize, PinTail: blockSize, CacheBytes: 1})
	for _, start := range []int{150, 10, 90, 10, 150, 60, 199, 0} {
		first, last := start*blockSize+7, min((start+40)*blockSize, len(data)-1)
		_, got := fetchRange(t, p.ForegroundURL(), fmt.Sprintf("bytes=%d-%d", first, last))
		if !bytes.Equal(got, data[first:last+1]) {
			t.Fatalf("window at block %d corrupted after eviction", start)
		}
	}
}

func TestProxyResumesAfterUpstreamReset(t *testing.T) {
	data := payload(12 * blockSize)
	u := newUpstream(t, data)
	u.dropAfter.Store(3*blockSize + 100)
	p := startProxy(t, Options{URL: u.srv.URL + "/m.mkv"})
	if _, got := fetchRange(t, p.ForegroundURL(), "bytes=0-"); !bytes.Equal(got, data) {
		t.Fatalf("body after reset: got %d bytes, want %d", len(got), len(data))
	}
	if n := u.requests.Load(); n != 2 {
		t.Fatalf("upstream requests = %d, want 2 (one silent resume)", n)
	}
}

func TestProxyRefreshesExpiredLink(t *testing.T) {
	data := payload(3 * blockSize)
	u := newUpstream(t, data)
	var refreshed atomic.Int64
	p := startProxy(t, Options{URL: u.srv.URL + "/old", Refresh: func(context.Context) (string, error) {
		refreshed.Add(1)
		return u.srv.URL + "/fresh.mkv", nil
	}})
	if code, got := fetchRange(t, p.ForegroundURL(), "bytes=10-"); code != http.StatusPartialContent || !bytes.Equal(got, data[10:]) {
		t.Fatalf("after refresh: code=%d len=%d", code, len(got))
	}
	if refreshed.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshed.Load())
	}
	noRefresh := startProxy(t, Options{URL: u.srv.URL + "/old"})
	if code, _ := fetchRange(t, noRefresh.ForegroundURL(), "bytes=0-"); code != http.StatusBadGateway {
		t.Fatalf("expired link without Refresh: code=%d, want 502", code)
	}
}

// A bulk background reader must make (almost) no progress while a foreground
// request is in flight, then run freely once it is gone.
func TestProxyBackgroundYieldsToForeground(t *testing.T) {
	data := payload(64 * blockSize)
	u := newUpstream(t, data)
	p := startProxy(t, Options{URL: u.srv.URL + "/m.mkv", PinHead: blockSize, PinTail: blockSize})

	foreground.Add(1) // a segment is being generated
	done := make(chan []byte, 1)
	go func() {
		_, b := fetchRange(t, p.BackgroundURL(), fmt.Sprintf("bytes=%d-", 2*blockSize))
		done <- b
	}()
	time.Sleep(400 * time.Millisecond)
	if held := p.Stats().UpstreamBytes; held > blockSize {
		t.Fatalf("background pulled %d bytes while foreground was active", held)
	}
	foreground.Add(-1)
	select {
	case got := <-done:
		if !bytes.Equal(got, data[2*blockSize:]) {
			t.Fatal("background body corrupted")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("background reader never resumed")
	}
}

func TestParseRange(t *testing.T) {
	for _, bad := range []string{"bytes=-5", "bytes=5-2", "bytes=a-", "items=0-", "bytes=0-1,4-5", "bytes=-"} {
		if _, _, _, ok := parseRange(bad); ok {
			t.Errorf("parseRange(%q) accepted", bad)
		}
	}
	if f, l, ranged, ok := parseRange(""); !ok || ranged || f != 0 || l != -1 {
		t.Errorf("empty range: %d %d %v %v", f, l, ranged, ok)
	}
}
