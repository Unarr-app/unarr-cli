package metrics

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

// ob is Observe for session "a" with the noise stripped.
func ob(m *Monitor, kind Kind, impaired bool, reason string, sec int) (Health, Stall) {
	h, st, _ := m.Observe(Tick{ID: "a", Kind: kind, Title: "t", Impaired: impaired, Reason: reason}, at(sec))
	return h, st
}

// A stall that lasts many ticks is ONE event and ONE count (Dispatcharr's
// deduplicated stall events), not one per sampling tick.
func TestMonitorOneEventPerStallNotPerTick(t *testing.T) {
	var events []StallEvent
	m := NewMonitor(30*time.Second, 10*time.Second, func(e StallEvent) { events = append(events, e) })

	for sec := 0; sec <= 120; sec += 5 { // 25 impaired ticks across 2 minutes
		ob(m, KindDownload, true, "no_data", sec)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1 for a single continuous stall", len(events))
	}
	if got := m.Totals()[KindDownload]; got != 1 {
		t.Fatalf("total = %d, want 1", got)
	}
	if events[0].Reason != "no_data" || events[0].ID != "a" || events[0].Title != "t" {
		t.Fatalf("event = %+v", events[0])
	}
}

func TestMonitorHealthProgression(t *testing.T) {
	m := NewMonitor(30*time.Second, 10*time.Second, nil)
	if h, _ := ob(m, KindHLS, false, "", 0); h != HealthOK {
		t.Fatalf("healthy = %s", h)
	}
	if h, _ := ob(m, KindHLS, true, "transcode", 5); h != HealthDegraded {
		t.Fatalf("inside grace = %s, want degraded", h)
	}
	h, st := ob(m, KindHLS, true, "transcode", 36)
	if h != HealthStalled || !st.Active || st.Count != 1 || st.LastAt == nil || st.LastReason != "transcode" {
		t.Fatalf("past grace = %s %+v", h, st)
	}
}

// A blip shorter than the grace never counts as a stall, and the clock restarts.
func TestMonitorShortBlipIsNotAStall(t *testing.T) {
	m := NewMonitor(30*time.Second, 10*time.Second, nil)
	ob(m, KindDownload, true, "x", 0)
	ob(m, KindDownload, true, "x", 20)
	ob(m, KindDownload, false, "", 25) // recovered in time
	ob(m, KindDownload, true, "x", 30) // new blip: clock restarts at 30
	h, st := ob(m, KindDownload, true, "x", 50)
	if h != HealthDegraded || st.Count != 0 {
		t.Fatalf("got %s count=%d, want degraded/0", h, st.Count)
	}
}

// A rate flapping around the floor during recovery stays ONE stall; only a
// sustained healthy run ends it, and the next stall is a new count.
func TestMonitorRecoveryHysteresis(t *testing.T) {
	m := NewMonitor(10*time.Second, 10*time.Second, nil)
	ob(m, KindMount, true, "r", 0)
	ob(m, KindMount, true, "r", 10) // stall #1
	ob(m, KindMount, false, "", 15) // healthy tick…
	h, st := ob(m, KindMount, true, "r", 20)
	if h != HealthStalled || st.Count != 1 {
		t.Fatalf("flap: %s count=%d, want still the same stall", h, st.Count)
	}
	ob(m, KindMount, false, "", 25)
	if h, _ = ob(m, KindMount, false, "", 36); h != HealthOK { // >=10s healthy
		t.Fatalf("after sustained recovery = %s", h)
	}
	ob(m, KindMount, true, "r", 40)
	_, st = ob(m, KindMount, true, "r", 50)
	if st.Count != 2 || m.Totals()[KindMount] != 2 {
		t.Fatalf("second stall count = %d total=%d, want 2/2", st.Count, m.Totals()[KindMount])
	}
}

func TestMonitorRetainKeepsTotals(t *testing.T) {
	m := NewMonitor(time.Second, time.Second, nil)
	ob(m, KindHLS, true, "r", 0)
	ob(m, KindHLS, true, "r", 2)
	m.Retain(map[string]bool{})
	if m.Totals()[KindHLS] != 1 {
		t.Fatal("totals must survive the session (monotonic counter)")
	}
	if _, st := ob(m, KindHLS, false, "", 3); st.Count != 0 {
		t.Fatal("a forgotten session starts with a clean history")
	}
}
