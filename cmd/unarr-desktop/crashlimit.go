package main

// Rate limiting and flap detection for crash reporting.
//
// Crashes were deduplicated by PID, which is sound for a daemon that dies once
// but not for one under a supervisor: systemd's Restart=always mints a fresh
// PID every few seconds, so a crash-looping agent looked like an endless series
// of distinct first-time crashes. A real box produced 25 reports in minutes and
// only stopped because the server rate-limited it — the client must not lean on
// that.
//
// So a restart loop is recognised as one event: reports are spaced out, and the
// tray says the agent is restarting repeatedly rather than flickering between
// "running" and "crashed" as each short-lived process comes and goes.

import "time"

const (
	// flapWindow is how far back a restart loop is measured.
	flapWindow = 10 * time.Minute
	// flapThreshold is how many crashes inside that window mean the agent is
	// looping rather than having crashed once. Three: two could be a crash and
	// a failed manual retry.
	flapThreshold = 3
	// crashReportInterval is the minimum spacing between crash reports. A loop
	// re-reports the same failure, so the developers need it once, not hourly —
	// but a genuinely separate crash later in the day is still worth hearing.
	crashReportInterval = time.Hour
	// crashHistory bounds what is remembered; only the flap window matters.
	crashHistory = 32
)

// crashTracker records recent crashes so a restart loop can be told from a
// one-off, and so reports are not sent faster than they are useful.
type crashTracker struct {
	at         []time.Time
	lastReport time.Time
}

// observe records a crash. The caller has already established this is a new
// crash and not one it has seen (PID dedupe).
func (c *crashTracker) observe(now time.Time) {
	c.at = append(c.at, now)
	if len(c.at) > crashHistory {
		c.at = c.at[len(c.at)-crashHistory:]
	}
}

// flapping reports whether the agent is in a restart loop rather than having
// crashed once.
func (c *crashTracker) flapping(now time.Time) bool {
	return c.countSince(now.Add(-flapWindow)) >= flapThreshold
}

// shouldReport reports whether this crash is worth telling the developers
// about, and records that it was. The first crash always goes; after that a
// loop is throttled to one report per interval.
func (c *crashTracker) shouldReport(now time.Time) bool {
	if !c.lastReport.IsZero() && now.Sub(c.lastReport) < crashReportInterval {
		return false
	}
	c.lastReport = now
	return true
}

func (c *crashTracker) countSince(cutoff time.Time) int {
	n := 0
	for _, t := range c.at {
		if t.After(cutoff) {
			n++
		}
	}
	return n
}

// crashStatusTitle is what the status row says about a dead agent: a loop is
// named as one, because "crashed" alternating with "running" every few seconds
// describes the symptom and hides the problem.
func crashStatusTitle(flapping bool) string {
	if flapping {
		return "Agent: restarting repeatedly"
	}
	return "Agent: crashed"
}

// crashStatusTooltip explains a loop, which the user cannot diagnose from a
// status row that keeps changing.
func crashStatusTooltip(flapping bool) string {
	if flapping {
		return "The agent keeps starting and exiting. Check its logs (View logs); " +
			"if its sign-in was rejected, use Sign in… to reconnect this machine."
	}
	return ""
}
