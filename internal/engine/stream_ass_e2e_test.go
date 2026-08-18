package engine

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

// End-to-end over the REAL handlers and a REAL fansub MKV: /sub (WebVTT, now
// drawing-filtered), /sub?f=ass (raw script) and /fonts (attachment dump).
// Skipped when the corpus file is not mounted.
const e2eMKV = "/mnt/nas/peliculas/TV Shows/The Exiled Heavy Knight Knows How to Game the System/Season 01/The Exiled Heavy Knight Knows How to Game the System - S01E01.mkv"

func newE2EServer(t *testing.T) *StreamServer {
	t.Helper()
	if _, err := os.Stat(e2eMKV); err != nil {
		t.Skip("corpus MKV not available")
	}
	ff, ok := mediainfo.LocateFFmpeg("")
	if !ok {
		t.Skip("ffmpeg unavailable")
	}
	return &StreamServer{
		streamSecret: []byte("e2e-secret-32-bytes-long-xxxxxxx"),
		requireToken: true,
		ffmpegPath:   ff,
		// Do not write sidecars next to the user's media during tests.
		cacheSubtitles: false,
	}
}

func TestE2ESubHandlerFiltersDrawings(t *testing.T) {
	ss := newE2EServer(t)
	tok := mintStreamToken(ss.streamSecret, streamScopeSub(e2eMKV, 0), time.Now())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sub?p="+url.QueryEscape(e2eMKV)+"&i=0&t="+tok, nil)
	ss.subtitleHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/sub → %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "WEBVTT") {
		t.Errorf("body is not WebVTT: %.60q", body)
	}
	// The bug this whole change exists for: the sign at 18:37 used to leak its
	// vector path as cue text.
	if strings.Contains(body, "m 0 0 l 290 0 290 42 0 42") {
		t.Error("drawing path still present in served WebVTT")
	}
	// The dialogue cues that SHARE that timestamp must survive.
	for _, want := range []string{"Rampart Reflection", "Common Skill"} {
		if !strings.Contains(body, want) {
			t.Errorf("served WebVTT lost legitimate cue %q", want)
		}
	}
	t.Logf("vtt: %d bytes, %d cues", len(body), strings.Count(body, "-->"))
}

func TestE2ESubHandlerServesRawASS(t *testing.T) {
	ss := newE2EServer(t)
	// Same token as the WebVTT request — the scope does not bind the format.
	tok := mintStreamToken(ss.streamSecret, streamScopeSub(e2eMKV, 0), time.Now())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sub?p="+url.QueryEscape(e2eMKV)+"&i=0&t="+tok+"&f=ass", nil)
	ss.subtitleHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/sub?f=ass → %d, want 200 (body: %.200s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/x-ssa; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/x-ssa; charset=utf-8", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"[Script Info]", "[V4+ Styles]", "[Events]", "PlayResX", "Dialogue:"} {
		if !strings.Contains(body, want) {
			t.Errorf("raw .ass missing %q", want)
		}
	}
	// The whole point: the drawing the WebVTT path had to throw away is intact,
	// with its override tags, so libass can draw the sign.
	if !strings.Contains(body, `\p1`) {
		t.Error("raw .ass lost the vector-drawing sign")
	}
	if !strings.Contains(body, "m 0 0 l 290 0 290 42 0 42") {
		t.Error("raw .ass lost the drawing path itself")
	}
	t.Logf("ass: %d bytes, %d dialogues", len(body), strings.Count(body, "Dialogue:"))
}

func TestE2EFontsHandlerDumpsRealAttachment(t *testing.T) {
	ss := newE2EServer(t)
	tok := mintStreamToken(ss.streamSecret, streamScopeFonts(e2eMKV), time.Now())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/fonts?p="+url.QueryEscape(e2eMKV)+"&i=1&n=arial.ttf&t="+tok, nil)
	ss.fontsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/fonts → %d, want 200 (body: %.200s)", rec.Code, rec.Body.String())
	}
	b := rec.Body.Bytes()
	if len(b) < 1000 {
		t.Fatalf("font is %d bytes, expected a real typeface", len(b))
	}
	if !(b[0] == 0x00 && b[1] == 0x01) && string(b[:4]) != "OTTO" && string(b[:4]) != "ttcf" {
		t.Errorf("served bytes are not a font, magic = % x", b[:4])
	}
	if ct := rec.Header().Get("Content-Type"); ct != "font/ttf" {
		t.Errorf("Content-Type = %q, want font/ttf", ct)
	}
	t.Logf("font: %d bytes, magic % x", len(b), b[:4])
}
