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
	if s.ActiveSec != 5 || s.AvgRateBps != 1200 {
		t.Fatalf("active=%d avg=%d, want 5 / 1200", s.ActiveSec, s.AvgRateBps)
	}
	if got := snap.Summary.BytesTotal[KindMount]; got != 5000 {
		t.Fatalf("bytes total = %d, want only the delta 5000", got)
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

// The first poll has no previous sample: zero flow must not read as a stall.
func TestHubFirstPollIsNotImpaired(t *testing.T) {
	rs := []Reading{{Session: Session{ID: "m", Kind: KindMount}, ExpectFlow: true}}
	snap := hubWith(&rs).Poll(at(0))
	if snap.Sessions[0].Health != HealthOK {
		t.Fatalf("health = %s", snap.Sessions[0].Health)
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
