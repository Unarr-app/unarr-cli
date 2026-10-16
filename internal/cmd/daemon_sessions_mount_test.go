package cmd

import (
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/metrics"
)

// fakeMount is the mount's read counters and clock, driven by the test.
type fakeMount struct {
	active, bytes int64
	now           time.Time
}

func (f *fakeMount) stats() (int64, int64) { return f.active, f.bytes }
func (f *fakeMount) clock() time.Time      { return f.now }

func newFakeMount() (*fakeMount, *mountSource) {
	f := &fakeMount{now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	return f, newMountSource(f.stats, f.clock)
}

func (f *fakeMount) advance(d time.Duration) { f.now = f.now.Add(d) }

// Reads arrive in bursts: between two polls there may be no read in flight at
// all. The session lives on through those gaps and ends only after a full
// grace period with neither a read in flight nor a byte served.
func TestMountSourceOutlivesGapsBetweenBursts(t *testing.T) {
	f, src := newFakeMount()
	if got := src.readings(); len(got) != 0 {
		t.Fatalf("idle mount reported as a session: %+v", got)
	}

	f.active, f.bytes = 1, 0
	got := src.readings()
	if len(got) != 1 || !got[0].ExpectFlow || !got[0].CumulativeBytes || got[0].ID != "mount" {
		t.Fatalf("read in flight = %+v, want one mount session expecting flow", got)
	}

	// Burst over, bytes served: still a session, but nothing in flight to stall.
	f.advance(5 * time.Second)
	f.active, f.bytes = 0, 5_000_000
	if got := src.readings(); len(got) != 1 || got[0].ExpectFlow || got[0].Bytes != 5_000_000 {
		t.Fatalf("between bursts = %+v, want the session kept and no flow expected", got)
	}

	f.advance(mountSessionGrace - time.Second)
	if got := src.readings(); len(got) != 1 {
		t.Fatalf("inside the grace: %+v, want the session kept", got)
	}

	f.advance(time.Second)
	if got := src.readings(); len(got) != 0 {
		t.Fatalf("grace elapsed: %+v, want the session ended", got)
	}
}

// A burst that starts and finishes between two polls is activity too: the
// counter moved even though no read is in flight when the poll looks.
func TestMountSourceCountsBurstBetweenPolls(t *testing.T) {
	f, src := newFakeMount()
	f.bytes = 1_000_000
	if got := src.readings(); len(got) != 1 || got[0].ExpectFlow {
		t.Fatalf("counter moved with no read in flight = %+v, want a session", got)
	}
	f.advance(mountSessionGrace)
	if got := src.readings(); len(got) != 0 {
		t.Fatalf("no activity for a whole grace = %+v, want no session", got)
	}
}

// Through the hub: the session keeps its start, its bytes and its average
// across the gaps; its rate is the real window's (0 when nothing moved), and a
// gap is never a stall however long the grace outlasts the stall grace.
func TestMountSessionKeepsCountersAcrossGaps(t *testing.T) {
	f, src := newFakeMount()
	hub := metrics.NewHub("v", nil)
	hub.AddSource(src.readings)
	hub.SetBytesCounter(metrics.KindMount, func() int64 { return f.bytes })
	start := f.now

	f.active = 1
	hub.Poll(f.now)
	f.advance(5 * time.Second)
	f.active, f.bytes = 0, 10_000_000 // burst done between polls
	hub.Poll(f.now)

	f.advance(5 * time.Second) // gap: nothing moved
	snap := hub.Poll(f.now)
	if len(snap.Sessions) != 1 {
		t.Fatalf("gap: %d sessions, want the mount kept", len(snap.Sessions))
	}
	s := snap.Sessions[0]
	if s.RateBps != 0 || s.Bytes != 10_000_000 || !s.StartedAt.Equal(start) || s.Health != metrics.HealthOK {
		t.Fatalf("gap: %+v, want rate 0, bytes kept, start kept, ok", s)
	}
	if s.AvgRateBps != 1_000_000 || snap.Summary.Active[metrics.KindMount] != 1 {
		t.Fatalf("gap: avg %d active %d, want 1 MB/s over 10 s and one mount", s.AvgRateBps, snap.Summary.Active[metrics.KindMount])
	}

	f.advance(4 * time.Second)
	f.active, f.bytes = 1, 14_000_000 // next burst
	s = hub.Poll(f.now).Sessions[0]
	if s.Bytes != 14_000_000 || s.RateBps != 1_000_000 || !s.StartedAt.Equal(start) {
		t.Fatalf("next burst: %+v, want counters continued, not restarted", s)
	}

	f.active = 0
	f.advance(mountSessionGrace)
	snap = hub.Poll(f.now)
	if len(snap.Sessions) != 0 || snap.Summary.BytesTotal[metrics.KindMount] != 14_000_000 {
		t.Fatalf("after the grace: %+v, want no session and the total kept", snap)
	}
}
