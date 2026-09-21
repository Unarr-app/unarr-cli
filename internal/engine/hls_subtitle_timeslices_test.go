package engine

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// windowRecorder is a fake extractor: it records the order windows are asked
// for and can hold one open until told to go on.
type windowRecorder struct {
	mu      sync.Mutex
	order   []int
	hold    map[int]chan struct{} // window → released when closed
	started chan int
	fail    map[int]int // window → how many times to fail first
}

func newWindowRecorder() *windowRecorder {
	return &windowRecorder{hold: map[int]chan struct{}{}, started: make(chan int, 64), fail: map[int]int{}}
}

func (r *windowRecorder) extract(ctx context.Context, start, _ float64) (map[int][]vttCue, error) {
	k := int(start / subtitleWindowSec)
	r.mu.Lock()
	r.order = append(r.order, k)
	gate := r.hold[k]
	failing := r.fail[k] > 0
	if failing {
		r.fail[k]--
	}
	r.mu.Unlock()
	r.started <- k
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if failing {
		return nil, errors.New("boom")
	}
	at := time.Duration(start*1000) * time.Millisecond
	return map[int][]vttCue{0: {{start: at + time.Second, end: at + 2*time.Second, text: "w"}}}, nil
}

func (r *windowRecorder) seen() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.order...)
}

// The field bug: a viewer who seeks to minute 15 must not wait for a 0→end pass
// to crawl there. The window under the new position goes next, the ones after
// it follow, and what was skipped is back-filled last.
func TestSubtitleWindowsFollowTheViewer(t *testing.T) {
	rec := newWindowRecorder()
	rec.hold[0] = make(chan struct{})
	w := newSubtitleWindows("[t]", 5*subtitleWindowSec, rec.extract)
	done := make(chan struct{})
	go func() { w.run(context.Background()); close(done) }()

	if k := <-rec.started; k != 0 {
		t.Fatalf("first window = %d, want 0", k)
	}
	w.seek(3*subtitleWindowSec + 1) // cancels window 0, which is NOT lost
	if k := <-rec.started; k != 3 {
		t.Fatalf("window after the seek = %d, want 3", k)
	}
	close(rec.hold[0]) // the retry of window 0 runs straight through
	<-done

	if got, want := rec.seen(), []int{0, 3, 4, 0, 1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("extraction order = %v, want %v", got, want)
	}
	cues := w.snapshot(0)
	if len(cues) != 5 {
		t.Fatalf("got %d cues, want one per window (5)", len(cues))
	}
	for i := 1; i < len(cues); i++ {
		if cues[i].start < cues[i-1].start {
			t.Fatalf("cues not in timeline order: %v", cues)
		}
	}
	if w.progress() != 5 {
		t.Fatalf("progress = %d, want 5", w.progress())
	}
}

// Seeking into a region that is already extracted has nothing to make room for.
func TestSubtitleWindowsSeekIntoCoveredRegionKeepsRunning(t *testing.T) {
	rec := newWindowRecorder()
	rec.hold[1] = make(chan struct{})
	w := newSubtitleWindows("[t]", 3*subtitleWindowSec, rec.extract)
	done := make(chan struct{})
	go func() { w.run(context.Background()); close(done) }()

	<-rec.started // 0, completes
	<-rec.started // 1, held
	w.seek(1)     // window 0: done
	close(rec.hold[1])
	<-done
	if got, want := rec.seen(), []int{0, 1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("extraction order = %v, want %v (window 1 must not be restarted)", got, want)
	}
}

// A window that keeps failing is given up on, so extraction still completes.
func TestSubtitleWindowsGiveUpOnAFailingWindow(t *testing.T) {
	rec := newWindowRecorder()
	rec.fail[1] = subtitleWindowAttempts
	w := newSubtitleWindows("[t]", 3*subtitleWindowSec, rec.extract)
	w.run(context.Background())
	if got, want := rec.seen(), []int{0, 1, 1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("extraction order = %v, want %v", got, want)
	}
	if len(w.snapshot(0)) != 2 || w.progress() != 3 {
		t.Fatalf("cues=%d progress=%d, want 2 and 3", len(w.snapshot(0)), w.progress())
	}
}

func TestSubtitleWindowsStopWithTheSession(t *testing.T) {
	rec := newWindowRecorder()
	rec.hold[0] = make(chan struct{})
	w := newSubtitleWindows("[t]", 3*subtitleWindowSec, rec.extract)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.run(ctx); close(done) }()
	<-rec.started
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after the session context ended")
	}
}

func TestSubtitleWindowBoundsCoverTheTimelineOnce(t *testing.T) {
	w := newSubtitleWindows("[t]", 2*subtitleWindowSec+7.5, nil)
	if len(w.done) != 3 {
		t.Fatalf("windows = %d, want 3", len(w.done))
	}
	if start, dur := w.bounds(2); start != 2*subtitleWindowSec || dur != 7.5 {
		t.Fatalf("last window = %v+%v, want %v+7.5", start, dur, 2*subtitleWindowSec)
	}
	var nilW *subtitleWindows
	nilW.seek(10) // sessions without text tracks have no extractor
}

func TestWindowCuesKeepOnlyTheirOwnAndShiftToSessionTime(t *testing.T) {
	sec := func(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
	in := []vttCue{
		{start: sec(898.47), end: sec(900.52), text: "previous window's, still showing at the seek point"},
		{start: sec(900), end: sec(902), text: "starts exactly on the window"},
		{start: sec(929.9), end: sec(933), text: "starts inside, ends after"},
		{start: sec(930), end: sec(931), text: "next window's"},
	}
	got := windowCues(append([]vttCue(nil), in...), 900, 930, false)
	if len(got) != 2 || got[0].start != sec(900) || got[1].end != sec(933) {
		t.Fatalf("windowCues = %+v", got)
	}
	// The last window has no successor to pick up a cue past the nominal end.
	if got := windowCues(append([]vttCue(nil), in...), 900, 930, true); len(got) != 3 {
		t.Fatalf("last window kept %d cues, want 3", len(got))
	}
}

func TestVTTCuesRoundTripWithStableIDs(t *testing.T) {
	src := "\xef\xbb\xbfWEBVTT\r\n\r\nNOTE hi\r\n\r\n00:05.050 --> 00:08.090 line:10%\r\n<i>Hola</i>\r\nmundo\r\n\r\n" +
		"01:00:00.000 --> 01:00:01.500\nSí.\n\nbad --> line\nx\n\n00:09.000 --> 00:09.000\nempty range\n"
	cues := parseVTTCues([]byte(src))
	if len(cues) != 2 {
		t.Fatalf("parsed %d cues, want 2: %+v", len(cues), cues)
	}
	if cues[0].start != 5050*time.Millisecond || cues[0].settings != "line:10%" || cues[0].text != "<i>Hola</i>\nmundo" {
		t.Fatalf("cue 0 = %+v", cues[0])
	}
	out := string(renderVTT(cues))
	want := "WEBVTT\n\n" + cues[0].id() + "\n00:00:05.050 --> 00:00:08.090 line:10%\n<i>Hola</i>\nmundo\n\n" +
		cues[1].id() + "\n01:00:00.000 --> 01:00:01.500\nSí.\n"
	if out != want {
		t.Fatalf("rendered:\n%s\nwant:\n%s", out, want)
	}
	if again := parseVTTCues([]byte(out)); !reflect.DeepEqual(again, cues) {
		t.Fatalf("round trip changed the cues: %+v", again)
	}
	if cues[0].id() == cues[1].id() || strings.ContainsAny(cues[0].id(), " \n") {
		t.Fatalf("bad ids %q %q", cues[0].id(), cues[1].id())
	}
}

func TestFrameClockReadsFramecrcTimestamps(t *testing.T) {
	var clock frameClock
	if _, ok := clock.position("0,     238238,     238238,       41,   518064, 0x4e18f41b"); ok {
		t.Fatal("a packet line before the #tb header has no known unit")
	}
	for _, line := range []string{"#software: Lavf60.16.100", "#tb 0: 1/1000", "#media_type 0: video", ""} {
		if _, ok := clock.position(line); ok {
			t.Errorf("position(%q) should not parse", line)
		}
	}
	got, ok := clock.position("0,     272981,     273023,       41,     8274, 0xf25e29c6, F=0x0")
	if !ok || got != 272981*time.Millisecond {
		t.Fatalf("position = %v,%v want 272.981s (the dts column)", got, ok)
	}
	clock.position("#tb 0: 1/90000")
	if got, _ := clock.position("0, 180000, 180000, 3750, 10, 0x0"); got != 2*time.Second {
		t.Fatalf("90 kHz timebase: position = %v, want 2s", got)
	}
}
