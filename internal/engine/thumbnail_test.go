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

func thumbReq(remoteAddr, query string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://stream.test/thumbnail"+query, nil)
	r.RemoteAddr = remoteAddr
	return r
}

func indexOfArg(ss []string, target string) int {
	for i, s := range ss {
		if s == target {
			return i
		}
	}
	return -1
}

// TestStreamScopeThumb_Vector pins the scope string against the web's
// TypeScript minter (tests/unit/stream-token.test.ts asserts the same vector).
// A token the web mints for a file MUST reduce to the same scope here or the
// thumbnail 404s.
func TestStreamScopeThumb_Vector(t *testing.T) {
	got := streamScopeThumb("/movies/Example (2020)/Example.mkv")
	const want = "thumb:d3f919154ea48832a0b52e4b4ca3e81185ea5f4e2b9e5fece32c6651908cbdd8"
	if got != want {
		t.Fatalf("streamScopeThumb mismatch (web parity broken!): got %q want %q", got, want)
	}
}

func TestStreamScopeThumb_DistinctPerPath(t *testing.T) {
	a := streamScopeThumb("/a.mkv")
	b := streamScopeThumb("/b.mkv")
	if a == b {
		t.Error("distinct paths produced the same thumb scope")
	}
	if streamScopeThumb("/a.mkv") != a {
		t.Error("same path produced a different thumb scope (not deterministic)")
	}
	if !strings.HasPrefix(a, "thumb:") || len(a) != len("thumb:")+64 {
		t.Errorf("scope %q is not thumb:<64 hex>", a)
	}
}

func TestBuildThumbnailArgs(t *testing.T) {
	args := buildThumbnailArgs("/x/movie.mkv", 123.5, 320)
	joined := strings.Join(args, " ")

	ssIdx, iIdx := indexOfArg(args, "-ss"), indexOfArg(args, "-i")
	if ssIdx < 0 || iIdx < 0 || ssIdx > iIdx {
		t.Errorf("-ss must precede -i (fast input seek): %v", args)
	}
	if args[ssIdx+1] != "123.500" {
		t.Errorf("pos arg = %q, want 123.500", args[ssIdx+1])
	}
	if args[iIdx+1] != "/x/movie.mkv" {
		t.Errorf("input arg = %q, want the path", args[iIdx+1])
	}
	for _, want := range []string{"-frames:v 1", "scale=320:-2", "-f mjpeg", "pipe:1", "-an", "-sn"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
}

func TestParseThumbPos(t *testing.T) {
	cases := map[string]float64{"": 0, "abc": 0, "-5": 0, "0": 0, "12.5": 12.5, "600": 600}
	for in, want := range cases {
		if got := parseThumbPos(in); got != want {
			t.Errorf("parseThumbPos(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseThumbWidth(t *testing.T) {
	cases := map[string]int{"": 320, "abc": 320, "10": 80, "5000": 640, "200": 200, "640": 640, "80": 80}
	for in, want := range cases {
		if got := parseThumbWidth(in); got != want {
			t.Errorf("parseThumbWidth(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestThumbnailHandler_MissingPath_400(t *testing.T) {
	srv := NewStreamServer(0)
	rec := httptest.NewRecorder()
	srv.thumbnailHandler(rec, thumbReq("198.51.100.7:40000", ""))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing path: status = %d, want 400", rec.Code)
	}
}

func TestThumbnailHandler_BadToken_404(t *testing.T) {
	srv := NewStreamServer(0)
	rec := httptest.NewRecorder()
	// Path present (so we pass the 400 gate) but a bogus token → 404, no oracle.
	srv.thumbnailHandler(rec, thumbReq("198.51.100.7:40000", "?p="+url.QueryEscape("/tmp/x.mkv")+"&t=deadbeef.0000"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("bad token: status = %d, want 404", rec.Code)
	}
}

func TestThumbnailHandler_ValidToken_NonexistentFile_404(t *testing.T) {
	srv := NewStreamServer(0)
	path := "/nonexistent/never-here.mkv"
	tok := mintStreamToken(srv.streamSecret, streamScopeThumb(path), time.Now())
	rec := httptest.NewRecorder()
	srv.thumbnailHandler(rec, thumbReq("198.51.100.7:40000", "?p="+url.QueryEscape(path)+"&t="+tok))
	if rec.Code != http.StatusNotFound {
		t.Errorf("valid token but missing file: status = %d, want 404 (regular-file clamp)", rec.Code)
	}
}

func TestThumbnailHandler_NoFFmpeg_503(t *testing.T) {
	srv := NewStreamServer(0) // ffmpegPath left empty
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(path, []byte("not really a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok := mintStreamToken(srv.streamSecret, streamScopeThumb(path), time.Now())
	rec := httptest.NewRecorder()
	srv.thumbnailHandler(rec, thumbReq("198.51.100.7:40000", "?p="+url.QueryEscape(path)+"&t="+tok))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no ffmpeg configured: status = %d, want 503", rec.Code)
	}
}

// TestThumbnailHandler_Success exercises the full success branch with a stub
// "ffmpeg" that writes JPEG magic bytes to stdout — no real ffmpeg/video
// needed. Validates 200 + image/jpeg + the body is passed through verbatim.
func TestThumbnailHandler_Success(t *testing.T) {
	srv := NewStreamServer(0)
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "ffmpeg.sh")
	// JPEG SOI marker (FF D8 FF) + filler, regardless of args.
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '\\377\\330\\377stub'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv.SetFFmpegPath(stub)

	tok := mintStreamToken(srv.streamSecret, streamScopeThumb(path), time.Now())
	rec := httptest.NewRecorder()
	srv.thumbnailHandler(rec, thumbReq("198.51.100.7:40000",
		"?p="+url.QueryEscape(path)+"&t="+tok+"&pos=10&w=200"))

	if rec.Code != http.StatusOK {
		t.Fatalf("stub ffmpeg: status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	if !strings.HasPrefix(rec.Body.String(), "\xff\xd8\xff") {
		t.Errorf("body missing JPEG magic bytes: %q", rec.Body.String())
	}
}
