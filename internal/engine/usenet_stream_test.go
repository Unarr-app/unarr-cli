package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/stream"
)

// dialFakeArticles stands up a fake NNTP server serving the given articles and
// returns a connected client (an ArticleFetcher). Connect seeds the pool so the
// first Body() doesn't block on an empty acquire channel. The client + server
// are torn down via t.Cleanup.
func dialFakeArticles(t *testing.T, articles map[string][]byte) *nntp.Client {
	t.Helper()
	fake := nntptest.NewFakeServer(t)
	fake.AddArticles(articles)
	client := nntp.NewClient(fake.Config())
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Connect(cctx); err != nil {
		t.Fatalf("nntp connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// serveAndReadAll fetches the whole registered /usenet source through a real
// StreamServer handler + minted token and returns the served bytes. It mirrors
// exactly how ffmpeg would consume the loopback URL.
func serveAndReadAll(t *testing.T, ss *StreamServer, id string) []byte {
	t.Helper()
	_, url := usenetFront(t, ss, id)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

func TestBuildUsenetStreamDirect(t *testing.T) {
	content := usenetTestData(40_000)
	n, articles := nntptest.BuildDirectFile("movie.2024.1080p.mkv", content, 4096)
	client := dialFakeArticles(t, articles)

	ss := NewStreamServer(0, 1)
	handle, err := BuildUsenetStream(context.Background(), client, n, ss, "sess-direct")
	if err != nil {
		t.Fatalf("BuildUsenetStream: %v", err)
	}
	if handle.Kind != stream.KindDirect {
		t.Fatalf("Kind = %s, want direct", handle.Kind)
	}
	if handle.VideoName != "movie.2024.1080p.mkv" {
		t.Fatalf("VideoName = %q", handle.VideoName)
	}
	if handle.VideoSize != int64(len(content)) {
		t.Fatalf("VideoSize = %d, want %d", handle.VideoSize, len(content))
	}
	if handle.Provider == nil {
		t.Fatal("handle.Provider is nil")
	}
	if handle.LoopbackURL == "" {
		t.Fatal("handle.LoopbackURL is empty")
	}
	if ss.ActiveUsenetSources() != 1 {
		t.Fatalf("registered sources = %d, want 1", ss.ActiveUsenetSources())
	}

	// The registered source must reproduce the exact bytes over the endpoint.
	if body := serveAndReadAll(t, ss, "sess-direct"); !bytes.Equal(body, content) {
		t.Fatalf("served %d bytes, want %d", len(body), len(content))
	}

	// Close unregisters; a second Close is a no-op.
	handle.Close()
	if ss.ActiveUsenetSources() != 0 {
		t.Fatalf("after Close: %d sources, want 0", ss.ActiveUsenetSources())
	}
	handle.Close()
}

func TestBuildUsenetStreamRarStore(t *testing.T) {
	content := usenetTestData(25_000)
	n, articles := nntptest.BuildRarStore("show.s01e01.mkv", content, 8000, 1200)
	client := dialFakeArticles(t, articles)

	ss := NewStreamServer(0, 1)
	handle, err := BuildUsenetStream(context.Background(), client, n, ss, "sess-rar")
	if err != nil {
		t.Fatalf("BuildUsenetStream (rar-store): %v", err)
	}
	if handle.Kind != stream.KindRarStore {
		t.Fatalf("Kind = %s, want rar-store", handle.Kind)
	}
	if handle.VideoName != "show.s01e01.mkv" {
		t.Fatalf("VideoName = %q, want show.s01e01.mkv", handle.VideoName)
	}
	if handle.VideoSize != int64(len(content)) {
		t.Fatalf("VideoSize = %d, want %d", handle.VideoSize, len(content))
	}
	// The video inside the STORE rar is served byte-exact across volume borders.
	if body := serveAndReadAll(t, ss, "sess-rar"); !bytes.Equal(body, content) {
		t.Fatalf("rar-store served %d bytes, want %d", len(body), len(content))
	}
}

// assertFallback asserts a non-streamable outcome: the ErrNotStreamable sentinel
// is returned and nothing was registered on the server.
func assertFallback(t *testing.T, ss *StreamServer, handle *UsenetStreamHandle, err error) {
	t.Helper()
	if handle != nil {
		t.Fatalf("expected nil handle on fallback, got %+v", handle)
	}
	if !errors.Is(err, stream.ErrNotStreamable) {
		t.Fatalf("err = %v, want ErrNotStreamable", err)
	}
	if ss.ActiveUsenetSources() != 0 {
		t.Fatalf("a non-streamable release registered %d sources, want 0", ss.ActiveUsenetSources())
	}
}

func TestBuildUsenetStreamPasswordFallback(t *testing.T) {
	content := usenetTestData(4_000)
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, 1000)
	n.Password = "hunter2" // NZB-level password => never stream
	client := dialFakeArticles(t, articles)

	ss := NewStreamServer(0, 1)
	handle, err := BuildUsenetStream(context.Background(), client, n, ss, "sess-pw")
	assertFallback(t, ss, handle, err)
}

func TestBuildUsenetStreamCompressedRarFallback(t *testing.T) {
	content := usenetTestData(12_000)
	n, articles := nntptest.BuildRarCompressed("movie.mkv", content, 5000, 1000)
	client := dialFakeArticles(t, articles)

	ss := NewStreamServer(0, 1)
	handle, err := BuildUsenetStream(context.Background(), client, n, ss, "sess-cmp")
	assertFallback(t, ss, handle, err)
}

func TestBuildUsenetStreamEncryptedRarFallback(t *testing.T) {
	content := usenetTestData(12_000)
	n, articles := nntptest.BuildRarEncrypted("movie.mkv", content, 5000, 1000)
	client := dialFakeArticles(t, articles)

	ss := NewStreamServer(0, 1)
	handle, err := BuildUsenetStream(context.Background(), client, n, ss, "sess-enc")
	assertFallback(t, ss, handle, err)
}

func TestBuildUsenetStreamNoVideoFallback(t *testing.T) {
	content := usenetTestData(3_000)
	n, articles := nntptest.BuildDirectFile("soundtrack.mp3", content, 800)
	client := dialFakeArticles(t, articles)

	ss := NewStreamServer(0, 1)
	handle, err := BuildUsenetStream(context.Background(), client, n, ss, "sess-audio")
	assertFallback(t, ss, handle, err)
}

func TestBuildUsenetStreamGuards(t *testing.T) {
	content := usenetTestData(2_000)
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, 800)
	client := dialFakeArticles(t, articles)

	// nil server → a hard setup error (NOT the ErrNotStreamable sentinel).
	if _, err := BuildUsenetStream(context.Background(), client, n, nil, "sess"); err == nil {
		t.Fatal("expected error for nil stream server")
	} else if errors.Is(err, stream.ErrNotStreamable) {
		t.Fatal("nil-server error must not be ErrNotStreamable")
	}

	// Invalid (path-traversal-shaped) source id → rejected before any registration.
	ss := NewStreamServer(0, 1)
	if _, err := BuildUsenetStream(context.Background(), client, n, ss, "bad/id"); err == nil {
		t.Fatal("expected error for invalid source id")
	}
	if ss.ActiveUsenetSources() != 0 {
		t.Fatalf("invalid id registered %d sources, want 0", ss.ActiveUsenetSources())
	}
}

// TestTryStreamUsenetGuards exercises the API-free early guards of the
// daemon-facing shell: they must reject before touching the web API / NNTP, so a
// downloader with no apiClient still returns a clean error (never panics).
func TestTryStreamUsenetGuards(t *testing.T) {
	u := NewUsenetDownloader(nil) // nil apiClient — guards must not reach it
	task := &Task{ID: "t1", NzbID: "abc"}

	if _, err := u.TryStreamUsenet(context.Background(), task, nil, "sess"); err == nil {
		t.Fatal("expected error for nil stream server")
	}

	ss := NewStreamServer(0, 1)
	if _, err := u.TryStreamUsenet(context.Background(), task, ss, "bad/id"); err == nil {
		t.Fatal("expected error for invalid source id")
	}
	if ss.ActiveUsenetSources() != 0 {
		t.Fatalf("guard path registered %d sources, want 0", ss.ActiveUsenetSources())
	}
}

func TestUsenetStreamHandleCloseNilSafe(t *testing.T) {
	var h *UsenetStreamHandle
	h.Close() // must not panic on a nil handle

	// A zero handle (no srv) is also safe to Close.
	(&UsenetStreamHandle{}).Close()
}

// compile-time: *nntp.Client must satisfy stream.ArticleFetcher (the seam the
// orchestrator relies on to share the batch download's connection pool).
var _ stream.ArticleFetcher = (*nntp.Client)(nil)
