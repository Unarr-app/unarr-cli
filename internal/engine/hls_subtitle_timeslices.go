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
)

// subtitleWindowAttempts is how often a window is tried before it is given up
// on: better a hole of one segment in the subtitles than retrying forever.
const subtitleWindowAttempts = 2

// extractWindowFunc returns the cues found reading [start, end), per subtitle
// track index, on the session timeline. Cues of neighbouring windows may come
// along (the read is wider than the window); the store dedupes them.
type extractWindowFunc func(ctx context.Context, start, end float64) (map[int][]vttCue, error)

type subtitleWindows struct {
	tag     string    // log prefix
	starts  []float64 // segment boundary table: window k is [starts[k], starts[k+1])
	extract extractWindowFunc

	mu       sync.Mutex
	done     []bool
	attempts []int
	nDone    int
	pending  map[int]struct{} // offered, not settled yet
	anchor   int              // window of the viewer's last seek
	running  int              // -1 when idle
	stop     context.CancelFunc
	cues     map[int][]vttCue            // by track index, sorted by start
	seen     map[int]map[string]struct{} // cue ids held, by track index
	first    chan struct{}               // closed once any window has been settled
	wake     chan struct{}               // cap 1: something was offered
}

func newSubtitleWindows(tag string, starts []float64, extract extractWindowFunc) *subtitleWindows {
	n := max(len(starts)-1, 0)
	return &subtitleWindows{
		tag: tag, starts: starts, extract: extract,
		done: make([]bool, n), attempts: make([]int, n), running: -1,
		pending: make(map[int]struct{}),
		cues:    make(map[int][]vttCue), seen: make(map[int]map[string]struct{}),
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
func (w *subtitleWindows) seek(k int) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.anchor = k
	if w.running >= 0 && (w.running < k || w.running > k+copyVODLookahead) {
		w.stop()
	}
}

// next is the pending window to extract now: the first one from the anchor on,
// else the earliest left behind it. -1 when nothing is pending.
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
// pending.
func (w *subtitleWindows) settle(k int, cues map[int][]vttCue, err error, cancelled bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.running, w.stop = -1, nil
	if err != nil {
		if cancelled {
			return
		}
		w.attempts[k]++
		log.Printf("%s subtitle window %d failed (attempt %d): %v", w.tag, k, w.attempts[k], err)
		if w.attempts[k] < subtitleWindowAttempts {
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

// merge adds the cues the track does not hold yet. Caller holds mu.
func (w *subtitleWindows) merge(track int, add []vttCue) {
	seen := w.seen[track]
	if seen == nil {
		seen = make(map[string]struct{})
		w.seen[track] = seen
	}
	merged := w.cues[track]
	for _, c := range add {
		id := c.id()
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
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

// snapshot is the cues extracted so far for one track.
func (w *subtitleWindows) snapshot(track int) []vttCue {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]vttCue(nil), w.cues[track]...)
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
