package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// windowedSession is a session whose track 0 is text, track 1 is bitmap, with
// windows 0 and 1 settled (one cue each) at progress 1 and 2.
func windowedSession(t *testing.T) *HLSSession {
	t.Helper()
	s := &HLSSession{probe: &StreamProbe{SubtitleTracks: []ProbeSubtitleTrack{
		{Index: 0, Codec: "ass"},
		{Index: 1, Codec: "hdmv_pgs_subtitle"},
	}}}
	s.copyCtx = context.Background()
	s.subWin = newSubtitleWindows("[t]", testSegStarts(10), nil)
	for k, text := range []string{"first", "second"} {
		at := time.Duration(k*6+1) * time.Second
		s.subWin.settle(k, map[int][]vttCue{0: {{start: at, end: at + time.Second, text: text}}}, nil, false)
	}
	return s
}

func getSidecar(s *HLSSession, idx int, query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.serveWindowedSubtitleVTT(rec, httptest.NewRequest(http.MethodGet, "/subs/s.vtt"+query, nil), idx)
	return rec
}

// The whole sidecar re-sent for every finished segment is O(n²) over a film. A
// player that says what it already has gets only what is new — and is told which
// progress the answer is current as of, so its next request can pick up there.
func TestSubtitleSidecarServesTheDeltaSinceAProgressValue(t *testing.T) {
	s := windowedSession(t)

	full := getSidecar(s, 0, "").Body.String()
	if !strings.Contains(full, "first") || !strings.Contains(full, "second") ||
		!strings.Contains(full, "\n"+vttProgressNote+"2\n") {
		t.Fatalf("full sidecar:\n%s", full)
	}
	delta := getSidecar(s, 0, "?since=1").Body.String()
	if strings.Contains(delta, "first") || !strings.Contains(delta, "second") ||
		!strings.Contains(delta, "\n"+vttProgressNote+"2\n") {
		t.Fatalf("delta since 1:\n%s", delta)
	}
	if upToDate := getSidecar(s, 0, "?since=2").Body.String(); strings.Contains(upToDate, "-->") {
		t.Fatalf("nothing is new since 2, got:\n%s", upToDate)
	}
	// Garbage is "I have nothing", never an error: a partial track is worse.
	if got := getSidecar(s, 0, "?since=abc").Body.String(); !strings.Contains(got, "first") {
		t.Fatalf("since=abc must serve everything, got:\n%s", got)
	}
	if got := len(parseVTTCues([]byte(full))); got != 2 {
		t.Fatalf("the progress note must not read as a cue: parsed %d cues", got)
	}
}

// A bitmap / unknown index used to wait out the first-window timer and then
// answer 200 with an empty track, which a player takes for "no dialogue".
func TestSubtitleSidecarUnknownTrackIs404AtOnce(t *testing.T) {
	s := windowedSession(t)
	s.subWin = newSubtitleWindows("[t]", testSegStarts(10), nil) // nothing settled: would block
	for _, idx := range []int{1, 7, -1} {
		began := time.Now()
		if rec := getSidecar(s, idx, ""); rec.Code != http.StatusNotFound {
			t.Errorf("track %d: status %d, want 404", idx, rec.Code)
		}
		if took := time.Since(began); took > time.Second {
			t.Errorf("track %d: answered after %s, want at once", idx, took)
		}
	}
}

// ffmpeg takes a response the proxy cut short for the end of the file and exits
// 0. Anywhere but at the file's tail that is a failed read, not a finished one.
func TestWindowReadCompleteRejectsAReadThatStoppedShort(t *testing.T) {
	sec := func(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
	const duration = 1400.0
	for _, c := range []struct {
		name     string
		got      time.Duration
		end      float64
		complete bool
	}{
		{"clock ran past the window", sec(908), 906, true},
		{"cut mid-window", sec(903), 906, false},
		{"no video packet at all", 0, 906, false},
		{"file's tail: video ends before the container does", sec(1393), 1400, true},
	} {
		if got := windowReadComplete(c.got, c.end, duration); got != c.complete {
			t.Errorf("%s: complete = %v, want %v", c.name, got, c.complete)
		}
	}
}

// fakeFFmpeg writes a shell script standing in for ffmpeg.
func fakeFFmpeg(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	path := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil { //nolint:gosec // G306: test stub must be executable.
		t.Fatal(err)
	}
	return path
}

const framecrcHead = `echo "#tb 0: 1/1000"`

func TestRunFFmpegUntilStopsAtTheClockAndReportsHowFarItGot(t *testing.T) {
	ctx := context.Background()

	// Reaches the limit, then would hang forever: it must be ended, as a success.
	hangs := fakeFFmpeg(t, framecrcHead+`; echo "0, 5000, 5000, 41, 9, 0x0"; echo "0, 9000, 9000, 41, 9, 0x0"; exec sleep 60`)
	began := time.Now()
	got, err := runFFmpegUntil(ctx, hangs, nil, 8*time.Second)
	if err != nil || got != 9*time.Second || time.Since(began) > 5*time.Second {
		t.Fatalf("clock reached: got=%v err=%v after %s", got, err, time.Since(began))
	}

	// Ends by itself short of the limit with exit 0: no error, but the caller
	// learns how far it got (windowReadComplete decides).
	short := fakeFFmpeg(t, framecrcHead+`; echo "0, 3000, 3000, 41, 9, 0x0"`)
	if got, err := runFFmpegUntil(ctx, short, nil, 8*time.Second); err != nil || got != 3*time.Second {
		t.Fatalf("short read: got=%v err=%v", got, err)
	}

	// A real failure carries ffmpeg's stderr.
	fails := fakeFFmpeg(t, `echo "Protocol not found" >&2; exit 8`)
	if _, err := runFFmpegUntil(ctx, fails, nil, time.Second); err == nil || !strings.Contains(err.Error(), "Protocol not found") {
		t.Fatalf("failure: err=%v", err)
	}

	// Cancelled from outside (a seek): reported as cancellation, not as failure.
	cctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(100*time.Millisecond, cancel)
	idle := fakeFFmpeg(t, framecrcHead+`; exec sleep 60`)
	if _, err := runFFmpegUntil(cctx, idle, nil, time.Second); err != context.Canceled {
		t.Fatalf("cancelled: err=%v, want context.Canceled", err)
	}
}
