package engine

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

// Prod, 30 days: 7 stream tasks over 4 users died before serving a byte with
//
//	debrid stream provider: debrid provider: unknown file size (HEAD gave nothing, no fallback)
//
// All of them were mode=stream against TorBox's beam-eu.torrin.app, and all of
// them had download_task.direct_file_size populated exact to the byte. Two
// things were wrong: the provider gave up when HEAD said nothing, and
// stream_handler passed fallbackSize 0 so the size we already had never
// reached it.

// muteHeadServer imitates that CDN: HEAD is refused outright, ranged GETs work
// perfectly.
func muteHeadServer(data []byte) (*httptest.Server, *int32) {
	var heads int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			atomic.AddInt32(&heads, 1)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		http.ServeContent(w, r, "movie.mp4", time.Time{}, bytes.NewReader(data))
	}))
	return srv, &heads
}

// deadServer answers nothing useful to any method — the worst case, where only
// the provider-listed size can save the stream.
func deadServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
}

func TestDebridProviderRangeProbeWhenHeadIsMute(t *testing.T) {
	data := makeData(9000)
	srv, heads := muteHeadServer(data)
	defer srv.Close()

	// fallbackSize 0 on purpose: the range probe alone must produce the size.
	p, err := NewDebridFileProvider(context.Background(), srv.URL, "movie.mp4", 0, nil)
	if err != nil {
		t.Fatalf("NewDebridFileProvider: %v", err)
	}
	if got := p.FileSize(); got != int64(len(data)) {
		t.Fatalf("FileSize = %d, want %d (from the Content-Range probe)", got, len(data))
	}
	if atomic.LoadInt32(heads) == 0 {
		t.Fatal("HEAD was never attempted — the probe must be the SECOND choice, not the first")
	}

	// A size is only useful if the stream actually serves against it.
	r := p.NewFileReader(context.Background())
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("served %d bytes, want %d and identical", len(got), len(data))
	}
}

func TestDebridProviderUsesProviderListedSizeWhenLinkIsFullyMute(t *testing.T) {
	srv := deadServer()
	defer srv.Close()

	// This is the exact prod shape: nothing can be learned from the link, but
	// the task carried the provider's size (e.g. 23346250742 for Obsession).
	const listed = int64(23346250742)
	p, err := NewDebridFileProvider(context.Background(), srv.URL, "movie.mp4", listed, nil)
	if err != nil {
		t.Fatalf("NewDebridFileProvider refused a task that had a known size: %v", err)
	}
	if got := p.FileSize(); got != listed {
		t.Fatalf("FileSize = %d, want the provider-listed %d", got, listed)
	}
}

func TestDebridProviderStillErrorsWithNoSizeAnywhere(t *testing.T) {
	srv := deadServer()
	defer srv.Close()

	// Nothing to serve against: refusing is correct — serving size 0 would hand
	// the browser an empty file.
	if _, err := NewDebridFileProvider(context.Background(), srv.URL, "movie.mp4", 0, nil); err == nil {
		t.Fatal("want an error when HEAD, the range probe and the fallback all fail")
	}
}

func TestDebridSizePrefersTheLinkOverTheListedSize(t *testing.T) {
	data := makeData(4096)

	// HEAD works: its answer wins over a (stale) listed size.
	srv, _ := rangeServer(data)
	defer srv.Close()
	p, err := NewDebridFileProvider(context.Background(), srv.URL, "movie.mp4", 999999, nil)
	if err != nil {
		t.Fatalf("NewDebridFileProvider: %v", err)
	}
	if got := p.FileSize(); got != int64(len(data)) {
		t.Fatalf("FileSize = %d, want the HEAD size %d, not the listed 999999", got, len(data))
	}

	// HEAD mute but the probe works: the probe also wins over the listed size.
	mute, _ := muteHeadServer(data)
	defer mute.Close()
	p2, err := NewDebridFileProvider(context.Background(), mute.URL, "movie.mp4", 999999, nil)
	if err != nil {
		t.Fatalf("NewDebridFileProvider (mute head): %v", err)
	}
	if got := p2.FileSize(); got != int64(len(data)) {
		t.Fatalf("FileSize = %d, want the probed size %d, not the listed 999999", got, len(data))
	}
}

// A 200 to "bytes=0-0" means Range was ignored. The probe must refuse it
// WITHOUT reading the body: that body is the whole file, and draining it to
// recycle the connection would burn the caller's entire budget downloading
// metered debrid bytes only to discard them.
func TestDebridRangeProbeRefusesA200WithoutDownloadingTheFile(t *testing.T) {
	const size = 64 << 20 // 64 MiB, big enough that a full read is unmistakable
	var served int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(size))
		w.WriteHeader(http.StatusOK)
		chunk := make([]byte, 32<<10)
		for sent := 0; sent < size; sent += len(chunk) {
			n, err := w.Write(chunk)
			atomic.AddInt64(&served, int64(n))
			if err != nil {
				return // client hung up — the expected outcome
			}
		}
	}))
	defer srv.Close()

	if gotSize, ok := debridRangeSize(context.Background(), srv.URL); ok {
		t.Fatalf("debridRangeSize accepted a 200 (size %d); an unverifiable length must not become the file size", gotSize)
	}
	// Some bytes are in flight before the close lands; the point is that the
	// whole file did NOT come across.
	if got := atomic.LoadInt64(&served); got > size/2 {
		t.Fatalf("probe pulled %d of %d bytes — the body was drained instead of dropped", got, size)
	}
}

// A soft-200 error page is the reason the rule above is not merely about
// bandwidth: its Content-Length is a real number, and trusting it would serve a
// 1.8 KB HTML body to the player as video with the task marked completed.
func TestDebridProviderRejectsASoft200ErrorPage(t *testing.T) {
	page := []byte(`<html><body>link expired</body></html>`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", fmt.Sprint(len(page)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(page)
	}))
	defer srv.Close()

	if _, err := NewDebridFileProvider(context.Background(), srv.URL, "movie.mp4", 0, nil); err == nil {
		t.Fatal("want a refusal: an error page's length must never become the advertised file size")
	}
}

// HEAD and the probe share the caller's budget. A CDN that HANGS on HEAD (as
// opposed to refusing it quickly) must not consume all of it, or the probe —
// the rung that exists for exactly these CDNs — never issues a request.
func TestDebridSizeProbeStillRunsWhenHeadHangs(t *testing.T) {
	data := makeData(4096)
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			<-r.Context().Done() // hang until the client gives up
			return
		}
		atomic.AddInt32(&gets, 1)
		http.ServeContent(w, r, "movie.mp4", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()

	// The budget the stream path actually grants the whole setup.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	p, err := NewDebridFileProvider(ctx, srv.URL, "movie.mp4", 0, nil)
	if err != nil {
		t.Fatalf("NewDebridFileProvider: %v (the probe never got a turn)", err)
	}
	if atomic.LoadInt32(&gets) == 0 {
		t.Fatal("no ranged GET was issued — HEAD ate the whole budget")
	}
	if got := p.FileSize(); got != int64(len(data)) {
		t.Fatalf("FileSize = %d, want %d", got, len(data))
	}
}

func TestTotalFromContentRange(t *testing.T) {
	cases := []struct {
		header string
		want   int64
		ok     bool
	}{
		{"bytes 0-0/1234", 1234, true},
		{"bytes 0-0/23346250742", 23346250742, true},
		{"bytes 0-0/ 4096 ", 4096, true},
		// Server doesn't know the total — must NOT be read as a size.
		{"bytes 0-0/*", 0, false},
		{"", 0, false},
		{"bytes 0-0", 0, false},
		{"bytes 0-0/0", 0, false},
		{"bytes 0-0/-5", 0, false},
		{"bytes 0-0/abc", 0, false},
	}
	for _, c := range cases {
		got, ok := totalFromContentRange(c.header)
		if got != c.want || ok != c.ok {
			t.Errorf("totalFromContentRange(%q) = (%d, %v), want (%d, %v)", c.header, got, ok, c.want, c.ok)
		}
	}
}
