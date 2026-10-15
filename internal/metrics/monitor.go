package metrics

import (
	"sync"
	"time"
)

// Stall thresholds, modelled on Dispatcharr's live proxy (buffering_timeout +
// a recovery hysteresis): a session is "degraded" the moment it is impaired,
// "stalled" once it has been impaired for GraceDefault, and the stall ends only
// after it has been healthy for RecoverDefault — so a rate flapping around the
// floor is one stall, not twenty.
const (
	GraceDefault   = 30 * time.Second
	RecoverDefault = 10 * time.Second
)

// Monitor turns a stream of per-tick observations into stall EPISODES: one
// event (and one count) per stall, however many ticks it lasts. Dispatcharr
// dedups its stall events the same way; without it a 5 s sampler would count a
// two-minute stall 24 times.
type Monitor struct {
	grace, recover time.Duration

	mu      sync.Mutex
	entries map[sessionKey]*episode
	// onStall, if set, runs once per stall start (outside the lock).
	onStall func(StallEvent)
	totals  map[Kind]int64
}

// StallEvent is emitted once when a session enters a stall.
type StallEvent struct {
	ID     string
	Kind   Kind
	Title  string
	Reason string
	At     time.Time
}

// sessionKey identifies a session. The id alone is not unique: a stream being
// watched while it downloads reports the download's task id.
type sessionKey struct {
	kind Kind
	id   string
}

func key(kind Kind, id string) sessionKey { return sessionKey{kind: kind, id: id} }

type episode struct {
	kind         Kind
	firstSeen    time.Time
	impairedFrom time.Time // zero = currently healthy
	healthyFrom  time.Time // start of the current healthy run while stalled
	stalled      bool
	count        int
	lastAt       time.Time
	lastReason   string
}

// NewMonitor builds a monitor. Non-positive durations take the defaults.
func NewMonitor(grace, recover time.Duration, onStall func(StallEvent)) *Monitor {
	if grace <= 0 {
		grace = GraceDefault
	}
	if recover <= 0 {
		recover = RecoverDefault
	}
	return &Monitor{
		grace: grace, recover: recover, onStall: onStall,
		entries: make(map[sessionKey]*episode),
		totals:  make(map[Kind]int64),
	}
}

// Tick is one observation of one session. Title and Reason only feed the
// emitted event.
type Tick struct {
	ID       string
	Kind     Kind
	Title    string
	Impaired bool
	Reason   string
}

// Observe records one tick for a session and returns its health, a copy of its
// stall history, and when the session was first seen.
func (m *Monitor) Observe(tk Tick, now time.Time) (Health, Stall, time.Time) {
	id, kind, impaired, reason := tk.ID, tk.Kind, tk.Impaired, tk.Reason
	k := key(kind, id)
	m.mu.Lock()
	e, ok := m.entries[k]
	if !ok {
		e = &episode{kind: kind, firstSeen: now}
		m.entries[k] = e
	}
	var fire *StallEvent
	switch {
	case impaired:
		e.healthyFrom = time.Time{}
		if e.impairedFrom.IsZero() {
			e.impairedFrom = now
		}
		if !e.stalled && now.Sub(e.impairedFrom) >= m.grace {
			e.stalled = true
			e.count++
			e.lastAt, e.lastReason = now, reason
			m.totals[kind]++
			fire = &StallEvent{ID: id, Kind: kind, Title: tk.Title, Reason: reason, At: now}
		}
	case e.stalled:
		// Recovering: only a sustained healthy run ends the stall.
		if e.healthyFrom.IsZero() {
			e.healthyFrom = now
		}
		if now.Sub(e.healthyFrom) >= m.recover {
			e.stalled = false
			e.impairedFrom = time.Time{}
			e.healthyFrom = time.Time{}
		}
	default:
		e.impairedFrom = time.Time{} // a blip shorter than the grace never counted
	}
	health := HealthOK
	switch {
	case e.stalled:
		health = HealthStalled
	case !e.impairedFrom.IsZero():
		health = HealthDegraded
	}
	st := Stall{Count: e.count, Active: e.stalled, LastReason: e.lastReason}
	if !e.lastAt.IsZero() {
		at := e.lastAt
		st.LastAt = &at
	}
	first := e.firstSeen
	m.mu.Unlock()

	if fire != nil && m.onStall != nil {
		m.onStall(*fire)
	}
	return health, st, first
}

// Retain forgets every session not in live. Totals survive: they are
// monotonic counters, and a Prometheus counter that drops on session end would
// read as a reset.
func (m *Monitor) Retain(live map[sessionKey]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.entries {
		if !live[k] {
			delete(m.entries, k)
		}
	}
}

// Totals returns stalls per kind since the monitor was created.
func (m *Monitor) Totals() map[Kind]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[Kind]int64, len(Kinds))
	for _, k := range Kinds {
		out[k] = m.totals[k]
	}
	return out
}
