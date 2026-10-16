package cmd

import (
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/metrics"
)

// mountSessionGrace is how long the mount's session outlives its last activity.
// A player reads in bursts — a few MB, then nothing for seconds — so most polls
// land between two reads; without the grace the session would end and restart
// (counters back to 0) on every gap.
const mountSessionGrace = 15 * time.Second

// mountSource reports the WebDAV mount as ONE session from its first read until
// mountSessionGrace has passed with no read in flight and no byte served. Safe
// for concurrent use (every poller calls it).
type mountSource struct {
	stats func() (active, bytes int64) // remotefs.ReadStats in the daemon
	now   func() time.Time

	mu         sync.Mutex
	seenBytes  int64     // the served-bytes counter at the previous call
	lastActive time.Time // last call that saw activity; zero = no session
}

func newMountSource(stats func() (active, bytes int64), now func() time.Time) *mountSource {
	return &mountSource{stats: stats, now: now}
}

// readings is the hub Source. Activity is a read in flight now, or bytes served
// since the previous call (a burst that began and ended between two polls). The
// byte counter is the process total: the hub turns it into the session's own
// bytes and keeps them, with the start time, for as long as the session lives.
// Only a read in flight expects flow — one that moves no bytes is what a stalled
// NNTP/remote fetch looks like from here — so a quiet grace is never a stall.
func (m *mountSource) readings() []metrics.Reading {
	active, bytes := m.stats()
	now := m.now()

	m.mu.Lock()
	if active > 0 || bytes > m.seenBytes {
		m.lastActive = now
	}
	m.seenBytes = max(m.seenBytes, bytes)
	live := !m.lastActive.IsZero() && now.Sub(m.lastActive) < mountSessionGrace
	if !live {
		m.lastActive = time.Time{}
	}
	m.mu.Unlock()

	if !live {
		return nil
	}
	return []metrics.Reading{{
		Session:         metrics.Session{ID: "mount", Kind: metrics.KindMount, Title: "WebDAV mount", State: "active", Bytes: bytes},
		ExpectFlow:      active > 0,
		Reason:          "no_data",
		CumulativeBytes: true,
	}}
}
