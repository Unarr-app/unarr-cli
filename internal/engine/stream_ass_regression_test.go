package engine

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

// Regression tests for the second review round. Each of these was a real
// defect, reproduced before it was fixed:
//
//   - f=ass on a subrip track declined correctly but then re-demuxed the whole
//     file with ffmpeg even though a fresh .vtt sidecar existed (the cache read
//     was guarded with !wantASS and never re-consulted), and the fallback
//     extraction ran on the ALREADY-EXHAUSTED 60s context of the ASS attempt.
//   - An external .srt sidecar was served verbatim under f=ass as
//     text/x-ssa — bytes libass cannot parse, a 200 with no subtitles.
//   - f=ass with no ffmpeg returned 503 even when a perfectly servable .vtt
//     sidecar was cached.

// buildSubripMKV muxes a minimal subrip-only MKV into dir and returns its path.
func buildSubripMKV(t *testing.T, ffmpegPath, dir string) string {
	t.Helper()
	srt := filepath.Join(dir, "in.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:02,000\nHola subrip\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mkv := filepath.Join(dir, "subrip.mkv")
	out, err := exec.Command(ffmpegPath, "-nostdin", "-loglevel", "error", //nolint:gosec // test fixture build
		"-i", srt, "-c:s", "srt", "-y", mkv).CombinedOutput()
	if err != nil {
		t.Skipf("cannot build subrip fixture: %v: %s", err, out)
	}
	return mkv
}

func newRegressionServer(t *testing.T, withFfmpeg bool) *StreamServer {
	t.Helper()
	ff := ""
	if withFfmpeg {
		var ok bool
		ff, ok = mediainfo.LocateFFmpeg("")
		if !ok {
			t.Skip("ffmpeg unavailable")
		}
	}
	return &StreamServer{
		streamSecret:   []byte("test-secret-32-bytes-long-xxxxxx"),
		requireToken:   true,
		ffmpegPath:     ff,
		cacheSubtitles: false,
	}
}

func seedVTTSidecar(t *testing.T, mediaPath string, index int, body string) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(mediaPath), ".unarr")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(mediaPath) + ".s0.vtt"
	if index != 0 {
		t.Fatalf("helper only seeds index 0, got %d", index)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAssDeclineServesCachedVTT(t *testing.T) {
	ss := newRegressionServer(t, true)
	dir := t.TempDir()
	media := buildSubripMKV(t, ss.ffmpegPath, dir)
	seedVTTSidecar(t, media, 0, "WEBVTT\n\n00:00.500 --> 00:01.500\nSENTINEL-FROM-CACHE\n")

	tok := mintStreamToken(ss.streamSecret, streamScopeSub(media, 0), time.Now())
	rec := httptest.NewRecorder()
	ss.subtitleHandler(rec, httptest.NewRequest(http.MethodGet,
		"/sub?p="+url.QueryEscape(media)+"&i=0&t="+tok+"&f=ass", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("f=ass decline → %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/vtt") {
		t.Errorf("decline served Content-Type %q, want text/vtt", ct)
	}
	if !strings.Contains(rec.Body.String(), "SENTINEL-FROM-CACHE") {
		t.Errorf("decline re-extracted instead of serving the fresh sidecar: %q", rec.Body.String())
	}
}

func TestAssDeclineWithoutFfmpegServesCachedVTT(t *testing.T) {
	// Build the fixture with a real ffmpeg, then serve WITHOUT one.
	ffReal, ok := mediainfo.LocateFFmpeg("")
	if !ok {
		t.Skip("ffmpeg unavailable")
	}
	dir := t.TempDir()
	media := buildSubripMKV(t, ffReal, dir)
	seedVTTSidecar(t, media, 0, "WEBVTT\n\n00:00.500 --> 00:01.500\nSENTINEL-NO-FFMPEG\n")

	ss := newRegressionServer(t, false)
	tok := mintStreamToken(ss.streamSecret, streamScopeSub(media, 0), time.Now())
	rec := httptest.NewRecorder()
	ss.subtitleHandler(rec, httptest.NewRequest(http.MethodGet,
		"/sub?p="+url.QueryEscape(media)+"&i=0&t="+tok+"&f=ass", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("f=ass without ffmpeg but with cached vtt → %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "SENTINEL-NO-FFMPEG") {
		t.Errorf("cached vtt not served: %q", rec.Body.String())
	}
}

func TestExternalSrtSidecarDeclinesToVTT(t *testing.T) {
	ss := newRegressionServer(t, true)
	dir := t.TempDir()
	srt := filepath.Join(dir, "movie.es.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:02,000\nHello plain srt\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tok := mintStreamToken(ss.streamSecret, streamScopeSub(srt, -1), time.Now())
	rec := httptest.NewRecorder()
	ss.subtitleHandler(rec, httptest.NewRequest(http.MethodGet,
		"/sub?p="+url.QueryEscape(srt)+"&i=-1&t="+tok+"&f=ass", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("external srt + f=ass → %d, want 200 (WebVTT fallback)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/vtt") {
		t.Errorf("srt bytes served as %q — a libass client gets an unparseable 'script'", ct)
	}
	if !strings.HasPrefix(rec.Body.String(), "WEBVTT") {
		t.Errorf("fallback body is not WebVTT: %.60q", rec.Body.String())
	}
}

func TestExternalAssSidecarStillServesRaw(t *testing.T) {
	ss := newRegressionServer(t, true)
	dir := t.TempDir()
	ass := filepath.Join(dir, "movie.es.ass")
	script := "[Script Info]\nScriptType: v4.00+\n\n[V4+ Styles]\nFormat: Name, Fontname\nStyle: Main,Arial\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:02.00,Main,,0,0,0,,Hola\n"
	if err := os.WriteFile(ass, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}

	tok := mintStreamToken(ss.streamSecret, streamScopeSub(ass, -1), time.Now())
	rec := httptest.NewRecorder()
	ss.subtitleHandler(rec, httptest.NewRequest(http.MethodGet,
		"/sub?p="+url.QueryEscape(ass)+"&i=-1&t="+tok+"&f=ass", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("external .ass + f=ass → %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/x-ssa") {
		t.Errorf("Content-Type %q, want text/x-ssa", ct)
	}
	if !strings.Contains(rec.Body.String(), "Style: Main") {
		t.Errorf("raw script lost its style table: %.80q", rec.Body.String())
	}
}
