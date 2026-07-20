package main

import (
	"testing"
	"time"
)

// base is a fixed clock: crash throttling is about elapsed time, so the tests
// drive it rather than sleeping.
var base = time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

func TestCrashTrackerReportsTheFirstCrash(t *testing.T) {
	var c crashTracker
	c.observe(base)

	if !c.shouldReport(base) {
		t.Error("the first crash must be reported")
	}
}

func TestCrashTrackerThrottlesARestartLoop(t *testing.T) {
	// The failure this exists for: systemd Restart=always mints a new PID every
	// few seconds, so PID dedupe never fires and a real box sent 25 reports in
	// minutes until the server rate-limited it.
	var c crashTracker
	now := base

	c.observe(now)
	if !c.shouldReport(now) {
		t.Fatal("the first crash of the loop must be reported")
	}

	sent := 0
	for range 30 { // half an hour of restarts, ten seconds apart
		now = now.Add(10 * time.Second)
		c.observe(now)
		if c.shouldReport(now) {
			sent++
		}
	}

	if sent != 0 {
		t.Errorf("sent %d further reports during the loop, want 0 — one failure is one report", sent)
	}
}

func TestCrashTrackerReportsAgainAfterTheInterval(t *testing.T) {
	// A genuinely separate crash later on is still worth hearing about; the
	// throttle must not silence the agent forever.
	var c crashTracker
	c.observe(base)
	c.shouldReport(base)

	later := base.Add(crashReportInterval + time.Minute)
	c.observe(later)

	if !c.shouldReport(later) {
		t.Error("a crash after the interval must be reported")
	}
}

func TestCrashTrackerDetectsFlapping(t *testing.T) {
	tests := []struct {
		name    string
		crashes []time.Duration // offsets from base
		at      time.Duration
		want    bool
	}{
		{
			name:    "a single crash is not a loop",
			crashes: []time.Duration{0},
			want:    false,
		},
		{
			name:    "two could be a crash and a failed retry",
			crashes: []time.Duration{0, time.Minute},
			want:    false,
		},
		{
			name:    "three inside the window is a loop",
			crashes: []time.Duration{0, 10 * time.Second, 20 * time.Second},
			at:      30 * time.Second,
			want:    true,
		},
		{
			// Crashes spread over days are not a loop, and the tray must go
			// back to plain "crashed" once the loop stops.
			name:    "old crashes fall out of the window",
			crashes: []time.Duration{0, time.Minute, 2 * time.Minute},
			at:      2 * time.Hour,
			want:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var c crashTracker
			for _, d := range tc.crashes {
				c.observe(base.Add(d))
			}
			if got := c.flapping(base.Add(tc.at)); got != tc.want {
				t.Errorf("flapping() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCrashTrackerHistoryStaysBounded(t *testing.T) {
	// The tray runs for weeks; a crash list that only grows is a leak.
	var c crashTracker
	for i := range 500 {
		c.observe(base.Add(time.Duration(i) * time.Second))
	}
	if len(c.at) > crashHistory {
		t.Errorf("kept %d crashes, want at most %d", len(c.at), crashHistory)
	}
}

func TestCrashStatusNamesTheLoop(t *testing.T) {
	// "crashed" alternating with "running" every few seconds describes the
	// symptom and hides the problem.
	if got := crashStatusTitle(true); got == crashStatusTitle(false) {
		t.Error("a restart loop must not read the same as a single crash")
	}
	if crashStatusTooltip(true) == "" {
		t.Error("a restart loop needs an explanation: the user cannot diagnose a row that keeps changing")
	}
	if crashStatusTooltip(false) != "" {
		t.Error("a plain crash needs no tooltip beyond the title")
	}
}
