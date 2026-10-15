package metrics

import (
	"context"
	"sort"
	"sync"
	"time"
)

// minRateWindow is the shortest span a derived rate is computed over. Polls
// closer together than this (a scrape racing the sampler) reuse the previous
// rate instead of dividing a few bytes by a few milliseconds.
const minRateWindow = time.Second

// Hub aggregates Sources into Snapshots. Safe for concurrent use.
type Hub struct {
	version string
	started time.Time
	mon     *Monitor

	mu      sync.Mutex
	sources []Source
	vpn     func() VPN
	states  func() map[string]int
	last    map[sessionKey]sample
	bytes   map[Kind]int64
}

type sample struct {
	bytes int64
	at    time.Time
	rate  int64
	// measured: rate comes from a real window between two samples. The first
	// sample stores rate 0 only as a placeholder, which must not read as no flow.
	measured bool
}

// NewHub builds a hub. onStall (may be nil) receives one event per stall.
func NewHub(version string, onStall func(StallEvent)) *Hub {
	return &Hub{
		version: version,
		started: time.Now(),
		mon:     NewMonitor(0, 0, onStall),
		last:    make(map[sessionKey]sample),
		bytes:   make(map[Kind]int64),
	}
}

// AddSource registers a session source.
func (h *Hub) AddSource(s Source) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sources = append(h.sources, s)
}

// SetVPN wires the VPN state reader.
func (h *Hub) SetVPN(fn func() VPN) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.vpn = fn
}

// SetDownloadStates wires the per-state download counter (running AND merely
// persisted downloads, which no session source reports).
func (h *Hub) SetDownloadStates(fn func() map[string]int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.states = fn
}

// Poll reads every source, advances the stall monitor and returns the snapshot.
// The daemon's sampler calls it on a fixed tick (that tick is what the stall
// grace is measured against); handlers call it for fresh data — extra calls are
// harmless because the monitor works on timestamps, not tick counts.
func (h *Hub) Poll(now time.Time) Snapshot {
	h.mu.Lock()
	sources, vpn, states := append([]Source(nil), h.sources...), h.vpn, h.states
	h.mu.Unlock()

	var readings []Reading
	for _, src := range sources {
		readings = append(readings, src()...)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	sessions := make([]Session, 0, len(readings))
	live := make(map[sessionKey]bool, len(readings))
	for _, r := range readings {
		live[key(r.Kind, r.ID)] = true
		sessions = append(sessions, h.finish(r, now))
	}
	h.mon.Retain(live)
	for k := range h.last {
		if !live[k] {
			delete(h.last, k)
		}
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		if sessions[i].Kind != sessions[j].Kind {
			return kindRank(sessions[i].Kind) < kindRank(sessions[j].Kind)
		}
		return sessions[i].StartedAt.Before(sessions[j].StartedAt)
	})

	snap := Snapshot{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   now.UTC(),
		Agent:         Agent{Version: h.version, UptimeSec: int64(now.Sub(h.started).Seconds())},
		Sessions:      sessions,
		Summary:       h.summarize(sessions),
	}
	if vpn != nil {
		snap.Summary.VPN = vpn()
	}
	if states != nil {
		snap.Summary.DownloadsByState = states()
	}
	return snap
}

// Run polls the hub every interval until ctx is done. The poll is what feeds
// the stall monitor: without a steady tick a stall would only be noticed when
// somebody happened to ask.
func (h *Hub) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			h.Poll(now)
		}
	}
}

// finish completes one reading: derived rate, byte accounting, stall health.
func (h *Hub) finish(r Reading, now time.Time) Session {
	s := r.Session
	prev, seen := h.last[key(s.Kind, s.ID)]
	var measured bool
	s.RateBps, measured = h.account(r, prev, seen, now)

	// A source that cannot yet tell (no measured window) must not look stalled.
	flowStopped := r.ExpectFlow && measured && s.RateBps == 0
	reason := r.Reason
	if reason == "" && flowStopped {
		reason = "no_data"
	}
	health, stall, first := h.mon.Observe(Tick{
		ID: s.ID, Kind: s.Kind, Title: s.Title,
		Impaired: r.Impaired || flowStopped, Reason: reason,
	}, now)
	s.Health, s.Stall = health, stall
	if s.StartedAt.IsZero() {
		s.StartedAt = first
	}
	s.StartedAt = s.StartedAt.UTC()
	if sec := int64(now.Sub(s.StartedAt).Seconds()); sec > 0 {
		s.ActiveSec = sec
		s.AvgRateBps = s.Bytes / sec
	}
	return s
}

// account returns the session's current rate (and whether it was measured over
// a real window) and, once per rate window, stores the new sample and credits
// the byte delta to the kind's monotonic total. Called with h.mu held.
func (h *Hub) account(r Reading, prev sample, seen bool, now time.Time) (int64, bool) {
	s := r.Session
	rate := s.RateBps
	if !r.RateKnown {
		rate = prev.rate
	}
	if seen && now.Sub(prev.at) < minRateWindow {
		return rate, prev.measured // too soon to re-sample: keep the previous window
	}
	if !r.RateKnown && seen {
		rate = 0
		if d := s.Bytes - prev.bytes; d > 0 {
			rate = int64(float64(d) / now.Sub(prev.at).Seconds())
		}
	}
	// Deltas only: the bytes a resumed download already held when first seen
	// were not moved by this daemon run.
	if seen && s.Bytes > prev.bytes {
		h.bytes[s.Kind] += s.Bytes - prev.bytes
	}
	h.last[key(s.Kind, s.ID)] = sample{bytes: s.Bytes, at: now, rate: rate, measured: seen}
	return rate, seen
}

func (h *Hub) summarize(sessions []Session) Summary {
	sum := Summary{
		Active:           make(map[Kind]int, len(Kinds)),
		BytesTotal:       make(map[Kind]int64, len(Kinds)),
		StallsTotal:      h.mon.Totals(),
		RateBps:          make(map[Kind]int64, len(Kinds)),
		DownloadsByState: map[string]int{},
	}
	for _, k := range Kinds {
		sum.Active[k] = 0
		sum.BytesTotal[k] = h.bytes[k]
		sum.RateBps[k] = 0
	}
	for _, s := range sessions {
		sum.Active[s.Kind]++
		sum.RateBps[s.Kind] += s.RateBps
		if s.Health == HealthStalled {
			sum.Stalled++
		}
	}
	return sum
}

func kindRank(k Kind) int {
	for i, x := range Kinds {
		if x == k {
			return i
		}
	}
	return len(Kinds)
}
