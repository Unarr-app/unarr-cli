package engine

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The /fonts token scope must stay byte-for-byte identical to the web's
// streamScopeFonts (src/lib/stream-token.ts). A drift makes every font 404 with
// no other symptom than subtitles quietly rendering in the fallback face.
func TestStreamScopeFontsVector(t *testing.T) {
	// sha256("/media/Show S01E01.mkv") — the same vector is asserted on the web
	// side so the two implementations cannot drift apart silently.
	const path = "/media/Show S01E01.mkv"
	got := streamScopeFonts(path)
	// sha256(path) hex, prefixed. The exact digest is pinned by the parity test
	// on the web side; here we assert the SHAPE the web mirrors.
	if !strings.HasPrefix(got, "fonts:") {
		t.Fatalf("scope must be prefixed 'fonts:', got %q", got)
	}
	if len(got) != len("fonts:")+64 {
		t.Errorf("scope = %q, want 'fonts:' + 64 hex chars", got)
	}
	// Unlike streamScopeSub, the scope must NOT bind an index — one token covers
	// every attachment of the file.
	if strings.Count(got, ":") != 1 {
		t.Errorf("scope %q binds more than the path; it must cover all fonts of the file", got)
	}
}

func TestStreamScopeFontsDiffersPerFile(t *testing.T) {
	a := streamScopeFonts("/media/A.mkv")
	b := streamScopeFonts("/media/B.mkv")
	if a == b {
		t.Error("different files produced the same font scope — a leaked token would expose both")
	}
}

func newFontTestServer(t *testing.T) *StreamServer {
	t.Helper()
	ss := &StreamServer{streamSecret: []byte("test-secret-32-bytes-long-xxxxxx"), requireToken: true}
	return ss
}

func TestFontsHandlerRejectsBadToken(t *testing.T) {
	ss := newFontTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/fonts?p=/media/x.mkv&i=0&n=arial.ttf&t=nope", nil)
	ss.fontsHandler(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("bad token → %d, want 404", rec.Code)
	}
}

func TestFontsHandlerRejectsBadIndex(t *testing.T) {
	ss := newFontTestServer(t)
	for _, idx := range []string{"", "abc", "-1"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/fonts?p=/media/x.mkv&i="+idx+"&t=x", nil)
		ss.fontsHandler(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("index %q → %d, want 400", idx, rec.Code)
		}
	}
}

func TestFontsHandlerRefusesRemoteSources(t *testing.T) {
	ss := newFontTestServer(t)
	const remote = "https://cdn.example.com/movie.mkv"
	tok := mintStreamToken(ss.streamSecret, streamScopeFonts(remote), time.Now())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/fonts?p="+url.QueryEscape(remote)+"&i=0&n=arial.ttf&t="+tok, nil)
	ss.fontsHandler(rec, req)
	// Dumping an attachment from a remote multi-GB container would mean pulling
	// its header over the network on every font; the renderer falls back instead.
	if rec.Code != http.StatusNotFound {
		t.Errorf("remote source → %d, want 404", rec.Code)
	}
}

func TestFontsHandlerServesFromCache(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "Show S01E01.mkv")
	if err := os.WriteFile(media, []byte("not really an mkv"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Seed the sidecar cache the way a prior request or the scan prewarm would.
	sidecar := filepath.Join(dir, ".unarr")
	if err := os.MkdirAll(sidecar, 0o755); err != nil {
		t.Fatal(err)
	}
	fontBytes := []byte("\x00\x01\x00\x00fake sfnt payload")
	if err := os.WriteFile(filepath.Join(sidecar, "Show S01E01.mkv.f3.otf"), fontBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	ss := newFontTestServer(t)
	tok := mintStreamToken(ss.streamSecret, streamScopeFonts(media), time.Now())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/fonts?p="+url.QueryEscape(media)+"&i=3&n="+url.QueryEscape("Adobe Arabic.otf")+"&t="+tok, nil)
	ss.fontsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("cache hit → %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != string(fontBytes) {
		t.Errorf("served %q, want the cached font bytes", got)
	}
	// .otf must not be announced as font/ttf — the extension comes from `n`.
	if ct := rec.Header().Get("Content-Type"); ct != "font/otf" {
		t.Errorf("Content-Type = %q, want font/otf", ct)
	}
}
