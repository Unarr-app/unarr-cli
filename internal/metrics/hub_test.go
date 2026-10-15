package metrics

import (
	"encoding/json"
	"testing"
	"time"
)

func hubWith(readings *[]Reading) *Hub {
	h := NewHub("9.9.9", nil)
	h.AddSource(func() []Reading { return *readings })
	return h
}

func TestHubDerivesRateAndAverage(t *testing.T) {
	rs := []Reading{{Session: Session{ID: "m", Kind: KindMount, State: "active", Bytes: 1000}}}
	h := hubWith(&rs)
	h.Poll(at(0))
	rs[0].Bytes = 6000
	snap := h.Poll(at(5))

	s := snap.Sessions[0]
	if s.RateBps != 1000 {
		t.Fatalf("rate = %d, want 1000 B/s", s.RateBps)
	}
	// The 1000 bytes held when first seen predate the session.
	if s.ActiveSec != 5 || s.AvgRateBps != 1000 {
		t.Fatalf("active=%d avg=%d, want 5 / 1000", s.ActiveSec, s.AvgRateBps)
	}
	if got := snap.Summary.BytesTotal[KindMount]; got != 5000 {
		t.Fatalf("bytes total = %d, want only the delta 5000", got)
	}
}

// The mount reports a process-wide counter and exists only while reads are in
// flight: the session's bytes start at zero when it opens, and the kind total
// follows the counter even across bursts no poll saw as a session.
func TestHubCumulativeCounterBytes(t *testing.T) {
	const gb, mb = int64(1) << 30, int64(1) << 20
	var counter int64
	active := false
	h := NewHub("v", nil)
	h.SetBytesCounter(KindMount, func() int64 { return counter })
	h.AddSource(func() []Reading {
		if !active {
			return nil
		}
		return []Reading{{Session: Session{ID: "mount", Kind: KindMount, State: "active", Bytes: counter}, CumulativeBytes: true, ExpectFlow: true}}
	})

	h.Poll(at(0))
	counter = 50 * gb // served in bursts that started and ended between ticks
	if got := h.Poll(at(5)).Summary.BytesTotal[KindMount]; got != 50*gb {
		t.Fatalf("bytes total after unseen bursts = %d, want %d", got, 50*gb)
	}

	active = true
	if s := h.Poll(at(10)).Sessions[0]; s.Bytes != 0 {
		t.Fatalf("bytes of a session that just opened = %d, want 0", s.Bytes)
	}
	counter += 10 * mb
	snap := h.Poll(at(20))
	s := snap.Sessions[0]
	if s.Bytes != 10*mb || s.AvgRateBps != mb || s.RateBps != mb {
		t.Fatalf("bytes=%d avg=%d rate=%d, want %d / %d / %d", s.Bytes, s.AvgRateBps, s.RateBps, 10*mb, mb, mb)
	}
	if got := snap.Summary.BytesTotal[KindMount]; got != 50*gb+10*mb {
		t.Fatalf("bytes total = %d, want %d", got, 50*gb+10*mb)
	}
}

// A resumed download already holds bytes from earlier runs: they stay in Bytes
// (that is the file's progress) but not in this session's average.
func TestHubResumedDownloadAverage(t *testing.T) {
	const gb, mb = int64(1) << 30, int64(1) << 20
	rs := []Reading{{Session: Session{ID: "d", Kind: KindDownload, State: "downloading", Bytes: 50 * gb, RateBps: mb}, RateKnown: true}}
	h := hubWith(&rs)
	h.Poll(at(0))
	rs[0].Bytes += 5 * mb
	snap := h.Poll(at(5))
	s := snap.Sessions[0]
	if s.Bytes != 50*gb+5*mb || s.AvgRateBps != mb {
		t.Fatalf("bytes=%d avg=%d, want %d / %d", s.Bytes, s.AvgRateBps, 50*gb+5*mb, mb)
	}
	if got := snap.Summary.BytesTotal[KindDownload]; got != 5*mb {
		t.Fatalf("bytes total = %d, want the delta %d", got, 5*mb)
	}
}

// Polls closer than the rate window must not divide a few bytes by a few
// milliseconds (a scrape racing the sampler).
func TestHubReusesRateInsideWindow(t *testing.T) {
	rs := []Reading{{Session: Session{ID: "m", Kind: KindMount, State: "active", Bytes: 0}}}
	h := hubWith(&rs)
	h.Poll(at(0))
	rs[0].Bytes = 5000
	h.Poll(at(5))
	rs[0].Bytes = 5001
	snap := h.Poll(at(5).Add(10 * time.Millisecond))
	if snap.Sessions[0].RateBps != 1000 {
		t.Fatalf("rate = %d, want the previous 1000", snap.Sessions[0].RateBps)
	}
}

func TestHubStallFromZeroFlowAndTotals(t *testing.T) {
	var events []StallEvent
	rs := []Reading{{
		Session:    Session{ID: "d1", Kind: KindDownload, State: "downloading"},
		RateKnown:  true,
		ExpectFlow: true,
		Reason:     "no_data",
	}}
	h := NewHub("v", func(e StallEvent) { events = append(events, e) })
	h.AddSource(func() []Reading { return rs })

	var snap Snapshot
	for sec := 0; sec <= 60; sec += 5 {
		snap = h.Poll(at(sec))
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	s := snap.Sessions[0]
	if s.Health != HealthStalled || !s.Stall.Active || s.Stall.Count != 1 {
		t.Fatalf("session = %+v", s)
	}
	if snap.Summary.Stalled != 1 || snap.Summary.StallsTotal[KindDownload] != 1 {
		t.Fatalf("summary = %+v", snap.Summary)
	}
}

// A stream being watched while it downloads reports the download's task id:
// the two sessions share an id and must not share rate samples or stall state.
func TestHubSameIDAcrossKindsStaysSeparate(t *testing.T) {
	dl := Reading{
		Session:    Session{ID: "task-1", Kind: KindDownload, State: "downloading", Bytes: 1_000_000},
		RateKnown:  true,
		ExpectFlow: true,
		Reason:     "no_data",
	}
	h := NewHub("v", nil)
	h.AddSource(func() []Reading { return []Reading{dl} })
	h.AddSource(func() []Reading {
		return []Reading{{Session: Session{ID: "task-1", Kind: KindStream, State: "active"}}}
	})

	var snap Snapshot
	for sec := 0; sec <= 35; sec += 5 { // impaired from the 2nd poll: 30 s of zero flow
		snap = h.Poll(at(sec))
	}
	if len(snap.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(snap.Sessions))
	}
	for _, s := range snap.Sessions {
		switch s.Kind {
		case KindDownload:
			if s.Health != HealthStalled || s.Stall.Count != 1 {
				t.Errorf("download = %s count %d, want stalled count 1", s.Health, s.Stall.Count)
			}
		case KindStream:
			if s.Health != HealthOK || s.RateBps != 0 {
				t.Errorf("stream = %s rate %d, want ok rate 0", s.Health, s.RateBps)
			}
		case KindHLS, KindMount:
			t.Errorf("unexpected session %+v", s)
		}
	}
	if got := snap.Summary.StallsTotal[KindDownload]; got != 1 {
		t.Fatalf("stallsTotal[download] = %d, want 1", got)
	}
}

// The first poll has no previous sample: zero flow must not read as a stall.
func TestHubFirstPollIsNotImpaired(t *testing.T) {
	rs := []Reading{{Session: Session{ID: "m", Kind: KindMount}, ExpectFlow: true}}
	snap := hubWith(&rs).Poll(at(0))
	if snap.Sessions[0].Health != HealthOK {
		t.Fatalf("health = %s", snap.Sessions[0].Health)
	}
}

// A second poll inside the first rate window (GET /sessions racing the sampler)
// has no measured rate yet: it must not read as zero flow while bytes move.
func TestHubUnmeasuredRateIsNotZeroFlow(t *testing.T) {
	rs := []Reading{{Session: Session{ID: "mount", Kind: KindMount, State: "active"}, ExpectFlow: true, Reason: "no_data"}}
	h := hubWith(&rs)
	h.Poll(at(0))
	rs[0].Bytes = 500_000
	if s := h.Poll(at(0).Add(500 * time.Millisecond)).Sessions[0]; s.Health != HealthOK {
		t.Fatalf("health 500 ms after the first sample = %s, want ok", s.Health)
	}

	// A real window with nothing moved still counts.
	rs[0].Bytes = 0
	h = hubWith(&rs)
	h.Poll(at(0))
	if s := h.Poll(at(5)).Sessions[0]; s.Health != HealthDegraded {
		t.Fatalf("health after 5 s of zero flow = %s, want degraded", s.Health)
	}
}

func TestHubSnapshotJSONContract(t *testing.T) {
	speed := 1.5
	rs := []Reading{{Session: Session{ID: "h1", Kind: KindHLS, Title: "Movie", State: "active", SpeedX: &speed}}}
	h := hubWith(&rs)
	h.SetVPN(func() VPN { return VPN{Enabled: true, Up: true} })
	h.SetDownloadStates(func() map[string]int { return map[string]int{"paused": 2} })
	raw, err := json.Marshal(h.Poll(at(0)))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schemaVersion", "generatedAt", "agent", "sessions", "summary"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing top-level key %q", k)
		}
	}
	if m["schemaVersion"].(float64) != SchemaVersion {
		t.Errorf("schemaVersion = %v", m["schemaVersion"])
	}
	sess := m["sessions"].([]any)[0].(map[string]any)
	for _, k := range []string{"id", "kind", "title", "state", "health", "startedAt", "activeSec", "bytes", "rateBps", "avgRateBps", "speedX", "stall"} {
		if _, ok := sess[k]; !ok {
			t.Errorf("missing session key %q", k)
		}
	}
	if _, ok := sess["progress"]; ok {
		t.Error("progress must be omitted for non-downloads")
	}
	sum := m["summary"].(map[string]any)
	for _, k := range []string{"active", "stalled", "bytesTotal", "stallsTotal", "rateBps", "downloadsByState", "vpn"} {
		if _, ok := sum[k]; !ok {
			t.Errorf("missing summary key %q", k)
		}
	}
	if len(sum["active"].(map[string]any)) != len(Kinds) {
		t.Errorf("summary.active must list every kind, got %v", sum["active"])
	}
}

func TestHubEmptyIsNotNull(t *testing.T) {
	raw, _ := json.Marshal(NewHub("v", nil).Poll(at(0)))
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	if string(m["sessions"]) != "[]" {
		t.Fatalf("sessions = %s, want []", m["sessions"])
	}
}
