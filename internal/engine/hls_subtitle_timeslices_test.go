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

// testSegSec is the segment length of the fake boundary tables below.
const testSegSec = 6.0

func testSegStarts(n int) []float64 {
	starts := make([]float64, n+1)
	for i := range starts {
		starts[i] = float64(i) * testSegSec
	}
	return starts
}

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

func (r *windowRecorder) extract(ctx context.Context, start, end float64) (map[int][]vttCue, error) {
	k := int(start / testSegSec)
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
	sec := func(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
	return map[int][]vttCue{0: {
		{start: sec(start + 1), end: sec(start + 2), text: "own"},
		// The read is wider than the window: the next window finds this one too.
		{start: sec(end + 0.5), end: sec(end + 1.5), text: "neighbour's"},
	}}, nil
}

func (r *windowRecorder) seen() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.order...)
}

func runWindows(t *testing.T, w *subtitleWindows) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.run(ctx); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("run did not return after the session context ended")
		}
	}
}

// Subtitles ride along with the video: a window is only ever read once its
// segment was generated (its bytes are then in the proxy cache). Nothing is
// downloaded for a part of the file nobody watches.
func TestSubtitleWindowsOnlyExtractOfferedSegments(t *testing.T) {
	rec := newWindowRecorder()
	w := newSubtitleWindows("[t]", testSegStarts(100), rec.extract)
	defer runWindows(t, w)()

	w.offer(0)
	w.offer(1)
	w.offer(1) // a retried request offers again
	waitFor(t, "both offered windows", func() bool { return w.progress() == 2 })
	time.Sleep(50 * time.Millisecond)
	if got, want := rec.seen(), []int{0, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("extracted %v, want %v and nothing else", got, want)
	}
	if got, want := w.covered(), [][2]float64{{0, 2 * testSegSec}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("covered = %v, want %v", got, want)
	}
	w.offer(0) // already done
	time.Sleep(50 * time.Millisecond)
	if len(rec.seen()) != 2 {
		t.Fatalf("a done window was extracted again: %v", rec.seen())
	}
}

// The field bug: after a seek to minute 15 the subtitles there must not queue
// behind anything. The window being read for the old position is set aside, the
// new position goes first, and what was left behind follows — it is not lost.
func TestSubtitleWindowsFollowTheViewer(t *testing.T) {
	rec := newWindowRecorder()
	rec.hold[0] = make(chan struct{})
	w := newSubtitleWindows("[t]", testSegStarts(200), rec.extract)
	defer runWindows(t, w)()

	w.offer(0)
	if k := <-rec.started; k != 0 {
		t.Fatalf("first window = %d, want 0", k)
	}
	w.offer(1)
	w.offer(2)
	w.offer(151) // prefetched past the new position
	w.offer(150)
	w.seek(150)
	if k := <-rec.started; k != 150 {
		t.Fatalf("window after the seek = %d, want 150", k)
	}
	close(rec.hold[0]) // the retry of window 0 runs straight through
	waitFor(t, "all five windows", func() bool { return w.progress() == 5 })

	if got, want := rec.seen(), []int{0, 150, 151, 0, 1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("extraction order = %v, want %v", got, want)
	}
	want := [][2]float64{{0, 3 * testSegSec}, {150 * testSegSec, 152 * testSegSec}}
	if got := w.covered(); !reflect.DeepEqual(got, want) {
		t.Fatalf("covered = %v, want %v", got, want)
	}
}

// Ordinary playback moves the position too; a window just ahead of it is
// exactly what is wanted next and must not be thrown away.
func TestSubtitleWindowsSeekNearbyKeepsTheRunningWindow(t *testing.T) {
	rec := newWindowRecorder()
	rec.hold[11] = make(chan struct{})
	w := newSubtitleWindows("[t]", testSegStarts(100), rec.extract)
	defer runWindows(t, w)()

	w.offer(11)
	<-rec.started
	w.seek(10)
	close(rec.hold[11])
	waitFor(t, "window 11", func() bool { return w.progress() == 1 })
	if got, want := rec.seen(), []int{11}; !reflect.DeepEqual(got, want) {
		t.Fatalf("extraction order = %v, want %v (window 11 must not be restarted)", got, want)
	}
}

// Neighbouring windows both turn up the cues around their seam; the viewer must
// get each once, in timeline order whatever order the windows came in.
func TestSubtitleWindowsDedupeAcrossSeams(t *testing.T) {
	rec := newWindowRecorder()
	w := newSubtitleWindows("[t]", testSegStarts(3), rec.extract)
	w.offer(2)
	w.offer(1)
	w.offer(0)
	w.run(context.Background()) // returns by itself: every window is done

	cues := w.snapshot(0)
	ids := map[string]bool{}
	for i, c := range cues {
		ids[c.id()] = true
		if i > 0 && c.start < cues[i-1].start {
			t.Fatalf("cues not in timeline order: %+v", cues)
		}
	}
	// 3 × "own" + 3 × "neighbour's" — all distinct; nothing doubled by the merge.
	if len(cues) != 6 || len(ids) != 6 {
		t.Fatalf("got %d cues / %d ids, want 6 distinct: %+v", len(cues), len(ids), cues)
	}
	w.mu.Lock()
	w.merge(0, cues) // the same cues again, as an overlapping read would
	w.mu.Unlock()
	if got := len(w.snapshot(0)); got != 6 {
		t.Fatalf("re-merging known cues grew the track to %d", got)
	}
}

// A window that keeps failing is given up on rather than retried forever.
func TestSubtitleWindowsGiveUpOnAFailingWindow(t *testing.T) {
	rec := newWindowRecorder()
	rec.fail[1] = subtitleWindowAttempts
	w := newSubtitleWindows("[t]", testSegStarts(3), rec.extract)
	for k := range 3 {
		w.offer(k)
	}
	w.run(context.Background())
	if got, want := rec.seen(), []int{0, 1, 1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("extraction order = %v, want %v", got, want)
	}
	if w.progress() != 3 {
		t.Fatalf("progress = %d, want 3 (the hole counts as settled)", w.progress())
	}
}

func TestSubtitleWindowsStopWithTheSession(t *testing.T) {
	rec := newWindowRecorder()
	rec.hold[0] = make(chan struct{})
	w := newSubtitleWindows("[t]", testSegStarts(3), rec.extract)
	stop := runWindows(t, w)
	w.offer(0)
	<-rec.started
	stop() // mid-extraction
	idle := newSubtitleWindows("[t]", testSegStarts(3), rec.extract)
	runWindows(t, idle)() // and while waiting for an offer

	var none *subtitleWindows // sessions without text tracks have no extractor
	none.offer(1)
	none.seek(1)
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
