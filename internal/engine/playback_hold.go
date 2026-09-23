package engine

import (
	"context"
	"sync"
	"time"
)

// PlaybackHold says whether the user is watching IPTV right now. IPTV accounts
// usually allow ONE connection, so an IPTV download must get out of the way
// while something plays and pick up again when it stops.
//
// The server reports the hold on every sync (`iptvHold`). It is a lease, not a
// switch: each "held" report extends it by ttl, and it lapses on its own when
// reports stop — an agent that lost the server (or a player that crashed
// without saying goodbye) must not keep a download paused forever.
type PlaybackHold struct {
	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	until time.Time     // zero = not held
	onSet chan struct{} // closed and replaced on every Set: wakes waiters to re-check
}

// NewPlaybackHold returns a released hold whose "held" reports last ttl.
func NewPlaybackHold(ttl time.Duration) *PlaybackHold {
	return &PlaybackHold{ttl: ttl, now: time.Now, onSet: make(chan struct{})}
}

// Set records the latest server report.
func (h *PlaybackHold) Set(held bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if held {
		h.until = h.now().Add(h.ttl)
	} else {
		h.until = time.Time{}
	}
	close(h.onSet)
	h.onSet = make(chan struct{})
}

// state returns whether the hold is in force, how long it lasts if nothing
// renews it, and the channel that closes on the next report.
func (h *PlaybackHold) state() (held bool, left time.Duration, changed <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	left = h.until.Sub(h.now())
	return !h.until.IsZero() && left > 0, left, h.onSet
}

// Held reports whether playback currently holds IPTV downloads.
func (h *PlaybackHold) Held() bool {
	held, _, _ := h.state()
	return held
}

// WaitReleased blocks until the hold is released or lapses, or ctx ends.
func (h *PlaybackHold) WaitReleased(ctx context.Context) error {
	for {
		held, left, changed := h.state()
		if !held {
			return nil
		}
		timer := time.NewTimer(left)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// WaitHeld blocks until the hold is in force, or ctx ends (returns ctx.Err()).
func (h *PlaybackHold) WaitHeld(ctx context.Context) error {
	for {
		held, _, changed := h.state()
		if held {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
