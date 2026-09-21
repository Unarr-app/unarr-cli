// Package engine — playhead-first subtitle extraction for COPY-VOD.
//
// A remote MKV interleaves its subtitle cues with the video, so getting them
// costs a read of the file. One 0→end pass meant a viewer who seeked to minute
// 15 had no subtitles until the pass crawled there: 88 s measured on a 24 min
// episode, many minutes on a film. The timeline is instead cut into short
// windows, each a bounded ffmpeg read, and the next window extracted is always
// the first missing one from where the viewer last seeked. Subtitles for the
// spot being watched arrive in one window's time, wherever that spot is.
package engine

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"
)

// subtitleWindowSec trades post-seek latency (one window must finish before its
// cues exist) against ffmpeg spawns, each of which re-parses the container
// header (served from the source proxy's pinned cache, so cheap but not free).
const subtitleWindowSec = 30.0

// subtitleWindowAttempts is how often a window is tried before it is given up
// on: better a 30 s hole in the subtitles than a loop that never completes.
const subtitleWindowAttempts = 2

// extractWindowFunc returns the cues starting in [start, start+dur), per
// subtitle track index, on the session timeline.
type extractWindowFunc func(ctx context.Context, start, dur float64) (map[int][]vttCue, error)

type subtitleWindows struct {
	tag     string // log prefix
	total   float64
	extract extractWindowFunc

	mu       sync.Mutex
	done     []bool
	attempts []int
	nDone    int
	anchor   int                // window of the viewer's last seek
	running  int                // -1 when idle
	stop     context.CancelFunc // cancels the running window
	cues     map[int][]vttCue   // by track index, sorted by start
	first    chan struct{}      // closed once any window has been settled
}

func newSubtitleWindows(tag string, total float64, extract extractWindowFunc) *subtitleWindows {
	n := int(total / subtitleWindowSec)
	if float64(n)*subtitleWindowSec < total {
		n++
	}
	return &subtitleWindows{
		tag: tag, total: total, extract: extract,
		done: make([]bool, n), attempts: make([]int, n), running: -1,
		cues: make(map[int][]vttCue), first: make(chan struct{}),
	}
}

// bounds is window k's start and length in seconds.
func (w *subtitleWindows) bounds(k int) (start, dur float64) {
	start = float64(k) * subtitleWindowSec
	dur = subtitleWindowSec
	if start+dur > w.total {
		dur = w.total - start
	}
	return start, dur
}

// seek moves extraction to the viewer's new position. A window being read
// somewhere else is abandoned — unless the new position is already covered, in
// which case there is nothing more urgent to make room for.
func (w *subtitleWindows) seek(sec float64) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	k := int(sec / subtitleWindowSec)
	if k < 0 || k >= len(w.done) {
		return
	}
	w.anchor = k
	if w.running >= 0 && w.running != k && !w.done[k] {
		w.stop()
	}
}

// claim picks the next window — the first missing one from the anchor on, then
// whatever was left behind it — and marks it running. k < 0: all done.
func (w *subtitleWindows) claim(ctx context.Context) (k int, wctx context.Context, cancel context.CancelFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(w.done)
	for i := 0; i < n; i++ {
		if c := (w.anchor + i) % n; !w.done[c] {
			wctx, cancel = context.WithCancel(ctx)
			w.running, w.stop = c, cancel
			return c, wctx, cancel
		}
	}
	return -1, nil, nil
}

// settle records the outcome of window k. A window cancelled by a seek is left
// missing so it is picked up again later.
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
		merged := append(w.cues[track], add...)
		sort.SliceStable(merged, func(i, j int) bool { return merged[i].start < merged[j].start })
		w.cues[track] = merged
	}
	w.done[k] = true
	w.nDone++
	if w.nDone == 1 {
		close(w.first)
	}
}

// run extracts every window, then returns. It also returns when ctx ends.
func (w *subtitleWindows) run(ctx context.Context) {
	began := time.Now()
	for {
		k, wctx, cancel := w.claim(ctx)
		if k < 0 {
			log.Printf("%s copy-vod subtitle sidecars complete (%d windows, %s)",
				w.tag, len(w.done), time.Since(began).Round(time.Second))
			return
		}
		start, dur := w.bounds(k)
		cues, err := w.extract(wctx, start, dur)
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
