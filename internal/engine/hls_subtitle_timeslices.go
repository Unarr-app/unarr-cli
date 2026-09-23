// Package engine — COPY-VOD subtitles ride along with the video segments.
//
// A remote MKV interleaves its subtitle cues with the video, so getting them
// costs a read of the file. That used to be a second, whole-file download
// running next to playback: a viewer who seeked to minute 15 had no subtitles
// until it crawled there (88 s measured on a 24 min episode, many minutes on a
// film), and an episode watched for five minutes was still fetched end to end.
//
// A player that demuxes the file itself never has this problem — the cues are in
// the bytes it already fetched for the picture. Same idea here: every window is
// one video segment, extracted right AFTER that segment was generated, when its
// bytes sit in the source proxy's cache. The read costs no network to speak of,
// the cues exist about when the picture does, and nothing is ever downloaded
// for a part of the file nobody watches.
package engine

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"
)

// subtitleWindowAttempts is how often a window is tried before it is given up
// on: better a hole of one segment in the subtitles than retrying forever.
const subtitleWindowAttempts = 3

// subtitleWindowRetryDelay spaces the attempts out. Retrying at once put every
// attempt inside the same network blip, turning a two-second hiccup into a
// permanent hole that was then reported as covered ("no dialogue here").
const subtitleWindowRetryDelay = 4 * time.Second

// extractWindowFunc returns the cues found reading [start, end), per subtitle
// track index, on the session timeline. Cues of neighbouring windows may come
// along (the read is wider than the window); the store dedupes them.
type extractWindowFunc func(ctx context.Context, start, end float64) (map[int][]vttCue, error)

type subtitleWindows struct {
	tag        string    // log prefix
	starts     []float64 // segment boundary table: window k is [starts[k], starts[k+1])
	extract    extractWindowFunc
	retryDelay time.Duration // between attempts at a failed window

	mu       sync.Mutex
	done     []bool
	attempts []int
	nDone    int
	pending  map[int]struct{} // offered, not settled yet
	anchor   int              // window of the viewer's last seek
	running  int              // -1 when idle
	stop     context.CancelFunc
	cues     map[int][]vttCue       // by track index, sorted by start
	seen     map[int]map[string]int // cue id → progress it appeared at, by track index
	first    chan struct{}          // closed once any window has been settled
	wake     chan struct{}          // cap 1: something was offered
}

func newSubtitleWindows(tag string, starts []float64, extract extractWindowFunc) *subtitleWindows {
	n := max(len(starts)-1, 0)
	return &subtitleWindows{
		tag: tag, starts: starts, extract: extract, retryDelay: subtitleWindowRetryDelay,
		done: make([]bool, n), attempts: make([]int, n), running: -1,
		pending: make(map[int]struct{}),
		cues:    make(map[int][]vttCue), seen: make(map[int]map[string]int),
		first: make(chan struct{}), wake: make(chan struct{}, 1),
	}
}

// offer queues window k: its video segment is on disk, so its bytes are as
// cheap to read as they will ever be.
func (w *subtitleWindows) offer(k int) {
	if w == nil {
		return
	}
	w.mu.Lock()
	if k >= 0 && k < len(w.done) && !w.done[k] {
		w.pending[k] = struct{}{}
	}
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// seek moves extraction to the viewer's new position: a window being read for
// the old one is set aside (it stays pending) so the new one goes first.
//
// It runs BEFORE the segment at k exists — the viewer's request is what
// generates it — so window k is typically not even offered yet. See next.
func (w *subtitleWindows) seek(k int) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if k < 0 || k >= len(w.done) {
		return
	}
	w.anchor = k
	if w.running >= 0 && (w.running < k || w.running > k+copyVODLookahead) {
		w.stop()
	}
}

// next is the pending window to extract now: the first one from the anchor on,
// else the earliest left behind it. -1 when there is nothing to do yet.
//
// Windows behind the anchor wait until the anchor's own window was dealt with.
// Without that, a seek set the running window aside only for it to be picked
// right back up (nothing at the new position is offered for a second or two),
// and the window the viewer is waiting for then queued behind it — behind a read
// that, off the cache, yields to the very segment fetches the seek just started.
func (w *subtitleWindows) next() int {
	ahead, behind := -1, -1
	for k := range w.pending {
		switch {
		case k >= w.anchor && (ahead < 0 || k < ahead):
			ahead = k
		case k < w.anchor && (behind < 0 || k < behind):
			behind = k
		}
	}
	if ahead >= 0 {
		return ahead
	}
	if anchorOpen := !w.done[w.anchor] && w.attempts[w.anchor] == 0; anchorOpen {
		return -1
	}
	return behind
}

// claim blocks until a window can be extracted and marks it running. k < 0:
// every window is done, or ctx ended.
func (w *subtitleWindows) claim(ctx context.Context) (k int, wctx context.Context, cancel context.CancelFunc) {
	for {
		w.mu.Lock()
		if w.nDone == len(w.done) {
			w.mu.Unlock()
			return -1, nil, nil
		}
		if k = w.next(); k >= 0 {
			wctx, cancel = context.WithCancel(ctx)
			w.running, w.stop = k, cancel
			w.mu.Unlock()
			return k, wctx, cancel
		}
		w.mu.Unlock()
		select {
		case <-w.wake:
		case <-ctx.Done():
			return -1, nil, nil
		}
	}
}

// settle records the outcome of window k. A window set aside by a seek stays
// pending; a failed one is taken off the queue and offered again after
// retryDelay, until its attempts run out and it is given up on (settled empty).
func (w *subtitleWindows) settle(k int, cues map[int][]vttCue, err error, cancelled bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.running, w.stop = -1, nil
	if err != nil {
		if cancelled {
			return
		}
		w.attempts[k]++
		log.Printf("%s subtitle window %d failed (attempt %d/%d): %v",
			w.tag, k, w.attempts[k], subtitleWindowAttempts, err)
		if w.attempts[k] < subtitleWindowAttempts {
			delete(w.pending, k)
			time.AfterFunc(w.retryDelay, func() { w.offer(k) })
			return
		}
	}
	for track, add := range cues {
		w.merge(track, add)
	}
	delete(w.pending, k)
	w.done[k] = true
	w.nDone++
	if w.nDone == 1 {
		close(w.first)
	}
}

// merge adds the cues the track does not hold yet, stamping each with the
// progress value it becomes visible at (settle bumps nDone right after), which is
// what lets snapshot answer "what is new since N". Caller holds mu.
func (w *subtitleWindows) merge(track int, add []vttCue) {
	seen := w.seen[track]
	if seen == nil {
		seen = make(map[string]int)
		w.seen[track] = seen
	}
	merged := w.cues[track]
	for _, c := range add {
		id := c.id()
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = w.nDone + 1
		merged = append(merged, c)
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].start < merged[j].start })
	w.cues[track] = merged
}

// run extracts offered windows until every window is done or ctx ends.
func (w *subtitleWindows) run(ctx context.Context) {
	for {
		k, wctx, cancel := w.claim(ctx)
		if k < 0 {
			if ctx.Err() == nil {
				log.Printf("%s copy-vod subtitle sidecars complete (%d windows)", w.tag, len(w.done))
			}
			return
		}
		cues, err := w.extract(wctx, w.starts[k], w.starts[k+1])
		cancelled := errors.Is(wctx.Err(), context.Canceled)
		cancel()
		if ctx.Err() != nil {
			return
		}
		w.settle(k, cues, err, cancelled)
	}
}

// snapshot is one track's cues that became visible after progress value `since`
// (0: all of them), in timeline order, with the progress they are current as of.
// A player topping a track up asks for the delta: the whole sidecar re-sent for
// every finished segment is O(n²) over a film — ~1200 fetches of a file that
// grows to 100 KB — and most of that would cross a Cloudflare tunnel.
func (w *subtitleWindows) snapshot(track, since int) (cues []vttCue, progress int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	seen := w.seen[track]
	for _, c := range w.cues[track] {
		if seen[c.id()] > since {
			cues = append(cues, c)
		}
	}
	return cues, w.nDone
}

// progress counts settled windows; it only ever grows, so a client can tell
// "there is something new to fetch" by comparing it with the last value it saw.
func (w *subtitleWindows) progress() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.nDone
}

// covered is the settled part of the timeline as merged [start, end] ranges, in
// seconds. Outside them a client cannot tell "no dialogue here" from "not
// extracted yet" — inside, it can.
func (w *subtitleWindows) covered() [][2]float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	ranges := [][2]float64{}
	for k, done := range w.done {
		if !done {
			continue
		}
		if n := len(ranges); n > 0 && ranges[n-1][1] == w.starts[k] {
			ranges[n-1][1] = w.starts[k+1]
			continue
		}
		ranges = append(ranges, [2]float64{w.starts[k], w.starts[k+1]})
	}
	return ranges
}
