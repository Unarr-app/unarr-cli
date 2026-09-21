package engine

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

func subtitleStreamSession(t *testing.T) (*HLSSession, string) {
	t.Helper()
	s := &HLSSession{tmpDir: t.TempDir(), subsDone: make(chan struct{})}
	if err := os.MkdirAll(filepath.Join(s.tmpDir, "subs"), 0o755); err != nil {
		t.Fatal(err)
	}
	return s, filepath.Join(s.tmpDir, "subs", "s0.vtt")
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func cue(sec int, text string) string {
	return fmt.Sprintf("00:%02d:%02d.000 --> 00:%02d:%02d.900\n%s\n\n", sec/60, sec%60, sec/60, sec%60, text)
}

// Field bug: the browser fetches a <track> once; served mid-extraction it kept
// only the cues read so far, so subtitles died after a seek past that point.
// The response must stay open and end up carrying EVERY cue.
func TestSubtitleVTTStreamsUntilExtractionCompletes(t *testing.T) {
	s, path := subtitleStreamSession(t)
	appendFile(t, path, "WEBVTT\n\n"+cue(1, "first"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.ServeSubtitleVTT(w, r, 0) }))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ContentLength >= 0 {
		t.Fatalf("growing sidecar served with a fixed Content-Length %d", resp.ContentLength)
	}
	body := bufio.NewReader(resp.Body)
	var got bytes.Buffer
	readUntil := func(marker string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(got.String(), marker) {
			if time.Now().After(deadline) {
				t.Fatalf("never received %q; have %q", marker, got.String())
			}
			line, err := body.ReadString('\n')
			got.WriteString(line)
			if err != nil {
				t.Fatalf("stream ended before %q: %v", marker, err)
			}
		}
	}

	// Progressive: the first cue is delivered while extraction is still running.
	readUntil("first")

	// A half-written cue must never be sent.
	appendFile(t, path, "00:15:00.000 --> 00:15:04.000\nminute fif")
	time.Sleep(3 * subtitleStreamPoll)
	appendFile(t, path, "teen\n\n")
	readUntil("minute fifteen")
	if strings.Contains(got.String(), "minute fif\n") {
		t.Fatalf("partial cue leaked: %q", got.String())
	}

	// ASS vector drawings leaked by ffmpeg are filtered in streamed chunks too.
	appendFile(t, path, "00:16:00.000 --> 00:16:01.000\nm 0 0 l 100 0 100 100 0 100\n\n"+cue(1020, "last"))
	close(s.subsDone) // extractor exits
	rest, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	got.Write(rest)

	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := string(mediainfo.FilterVTTDrawingCues(full))
	if got.String() != want {
		t.Fatalf("streamed body differs from the finished, filtered sidecar\n got: %q\nwant: %q", got.String(), want)
	}
	if strings.Contains(got.String(), "m 0 0 l") {
		t.Fatal("drawing cue was not filtered from the stream")
	}
}

func TestSubtitleVTTOneShotOnceExtractionIsDone(t *testing.T) {
	s, path := subtitleStreamSession(t)
	appendFile(t, path, "WEBVTT\n\n"+cue(1, "a")+cue(900, "b"))
	close(s.subsDone)
	rec := httptest.NewRecorder()
	s.ServeSubtitleVTT(rec, httptest.NewRequest(http.MethodGet, "/", nil), 0)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Length") == "" || !strings.Contains(rec.Body.String(), "b") {
		t.Fatalf("finished sidecar: code=%d len=%q", rec.Code, rec.Header().Get("Content-Length"))
	}
}

func TestSubtitleVTTStreamStopsWhenClientLeaves(t *testing.T) {
	s, path := subtitleStreamSession(t)
	appendFile(t, path, "WEBVTT\n\n"+cue(1, "a"))
	returned := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.ServeSubtitleVTT(w, r, 0)
		close(returned)
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("handler kept streaming to a client that left")
	}
}
