package engine

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/stream"
)

// usenetTestData builds a deterministic, non-trivial byte pattern of length n.
func usenetTestData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*37 + 11) & 0xff)
	}
	return b
}

// newUsenetProvider spins up a fake NNTP server serving a single directly-posted
// video, builds the streamability plan against it, and returns a FileProvider
// ready to register on a StreamServer. The returned content is the exact bytes
// the provider must reproduce over HTTP.
func newUsenetProvider(t *testing.T) (FileProvider, []byte) {
	t.Helper()
	const size = 100_000
	content := usenetTestData(size)
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, 16_384)

	fake := nntptest.NewFakeServer(t)
	fake.AddArticles(articles)
	client := nntp.NewClient(fake.Config())
	// Connect seeds the connection pool; without it Body() blocks forever in
	// acquire() (the pool channel is empty). Mirrors the stream package's
	// dialFake helper. The client stays alive for the whole test so the
	// provider's per-request readers can fetch articles.
	connCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Connect(connCtx); err != nil {
		t.Fatalf("nntp connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	plan := stream.StreamPlanFromNZB(context.Background(), client, n)
	if !plan.Streamable() {
		t.Fatalf("expected a streamable plan, got kind=%s reason=%q", plan.Kind, plan.Reason)
	}
	if plan.Kind != stream.KindDirect {
		t.Fatalf("kind = %s, want direct", plan.Kind)
	}
	if plan.VideoSize != int64(size) {
		t.Fatalf("VideoSize = %d, want %d", plan.VideoSize, size)
	}
	prov := NewUsenetFileProvider(plan.VideoName, plan.VideoSize, plan.Open)
	if prov == nil {
		t.Fatal("NewUsenetFileProvider returned nil for a streamable plan")
	}
	return prov, content
}

// usenetFront wires ss.usenetHandler behind an httptest server and mints a valid
// token for id, mirroring how ffmpeg would fetch /usenet/<id>?t=<token>.
func usenetFront(t *testing.T, ss *StreamServer, id string) (*httptest.Server, string) {
	t.Helper()
	front := httptest.NewServer(http.HandlerFunc(ss.usenetHandler))
	t.Cleanup(front.Close)
	token := mintStreamToken(ss.streamSecret, streamScopeUsenet(id), time.Now())
	return front, front.URL + "/usenet/" + id + "?t=" + token
}

func TestUsenetProviderBasics(t *testing.T) {
	prov, content := newUsenetProvider(t)
	if prov.FileName() != "movie.mkv" {
		t.Fatalf("FileName = %q, want movie.mkv", prov.FileName())
	}
	if prov.FileSize() != int64(len(content)) {
		t.Fatalf("FileSize = %d, want %d", prov.FileSize(), len(content))
	}
	// NewUsenetFileProvider must reject a nil opener (a non-streamable plan).
	if NewUsenetFileProvider("x.mkv", 1, nil) != nil {
		t.Fatal("expected nil provider for a nil opener")
	}
}

func TestUsenetRegistryLifecycle(t *testing.T) {
	prov, _ := newUsenetProvider(t)
	ss := NewStreamServer(0, 1)
	if ss.ActiveUsenetSources() != 0 {
		t.Fatalf("fresh server has %d sources, want 0", ss.ActiveUsenetSources())
	}
	ss.RegisterUsenetSource("s1", prov)
	if ss.ActiveUsenetSources() != 1 {
		t.Fatalf("after register: %d sources, want 1", ss.ActiveUsenetSources())
	}
	if ss.usenet.get("s1") == nil {
		t.Fatal("registered source not retrievable")
	}
	// Nil provider is refused, not stored.
	ss.RegisterUsenetSource("s2", nil)
	if ss.ActiveUsenetSources() != 1 {
		t.Fatalf("nil provider was stored: %d sources, want 1", ss.ActiveUsenetSources())
	}
	ss.UnregisterUsenetSource("s1")
	if ss.ActiveUsenetSources() != 0 {
		t.Fatalf("after unregister: %d sources, want 0", ss.ActiveUsenetSources())
	}
	ss.UnregisterUsenetSource("does-not-exist") // no-op, must not panic
}

func TestUsenetEndpointFullGET(t *testing.T) {
	prov, content := newUsenetProvider(t)
	ss := NewStreamServer(0, 1)
	ss.RegisterUsenetSource("full", prov)
	_, url := usenetFront(t, ss, "full")

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/x-matroska" && ct != "video/mkv" {
		// mimeTypeFromExt maps .mkv; accept either common label.
		t.Logf("Content-Type = %q (informational)", ct)
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes", resp.Header.Get("Accept-Ranges"))
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, content) {
		t.Fatalf("full GET body mismatch: got %d bytes, want %d", len(body), len(content))
	}
}

func TestUsenetEndpointRange206(t *testing.T) {
	prov, content := newUsenetProvider(t)
	ss := NewStreamServer(0, 1)
	ss.RegisterUsenetSource("ranged", prov)
	_, url := usenetFront(t, ss, "ranged")

	const lo, hi = 10_000, 19_999
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Range", "bytes="+strconv.Itoa(lo)+"-"+strconv.Itoa(hi))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ranged GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, content[lo:hi+1]) {
		t.Fatalf("range body mismatch: got %d bytes, want %d", len(body), hi-lo+1)
	}
}

func TestUsenetEndpointHEADReportsSize(t *testing.T) {
	prov, content := newUsenetProvider(t)
	ss := NewStreamServer(0, 1)
	ss.RegisterUsenetSource("head", prov)
	_, url := usenetFront(t, ss, "head")

	req, _ := http.NewRequest(http.MethodHead, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(content)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(content))
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Fatalf("HEAD returned %d body bytes, want 0", len(body))
	}
}

func TestUsenetEndpointNoTokenUnauthorized(t *testing.T) {
	prov, _ := newUsenetProvider(t)
	ss := NewStreamServer(0, 1)
	ss.RegisterUsenetSource("secure", prov)
	front, _ := usenetFront(t, ss, "secure")

	// No ?t= token → 401, even though the source exists.
	resp, err := http.Get(front.URL + "/usenet/secure")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	// A token minted for a DIFFERENT id must not authorise this one.
	wrong := mintStreamToken(ss.streamSecret, streamScopeUsenet("other"), time.Now())
	resp2, err := http.Get(front.URL + "/usenet/secure?t=" + wrong)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cross-id token status = %d, want 401", resp2.StatusCode)
	}
}

func TestUsenetEndpointUnknownSource404(t *testing.T) {
	ss := NewStreamServer(0, 1)
	front, url := usenetFront(t, ss, "ghost") // valid token, nothing registered
	_ = front
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unregistered id", resp.StatusCode)
	}
}

func TestUsenetEndpointBadIDRejected(t *testing.T) {
	ss := NewStreamServer(0, 1)
	front := httptest.NewServer(http.HandlerFunc(ss.usenetHandler))
	defer front.Close()
	// A path-traversal-shaped id fails the validSessionID gate before any lookup.
	resp, err := http.Get(front.URL + "/usenet/bad%2Fid")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for malformed id", resp.StatusCode)
	}
}

func TestUsenetLoopbackURLTokenised(t *testing.T) {
	ss := NewStreamServer(4321, 1) // port not bound; URL is built from ss.port
	url := ss.UsenetLoopbackURL("abc")
	if url == "" {
		t.Fatal("empty URL for a non-empty id")
	}
	const wantPrefix = "http://127.0.0.1:4321/usenet/abc?t="
	if len(url) <= len(wantPrefix) || url[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("URL = %q, want prefix %q with a token", url, wantPrefix)
	}
	if ss.UsenetLoopbackURL("") != "" {
		t.Fatal("expected empty URL for empty id")
	}
}
