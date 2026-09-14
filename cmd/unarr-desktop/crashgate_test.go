package main

import (
	"testing"
	"time"
)

// TestCrashIsClaimedOncePerRun: the on-disk verdict survives the process that
// made it, and is keyed by run rather than by PID.
func TestCrashIsClaimedOncePerRun(t *testing.T) {
	isolatePaths(t)
	run := agentStatus{crashed: true, pid: 14112, startedAt: time.Date(2026, 9, 14, 13, 0, 0, 0, time.UTC)}

	if !claimCrashReport(run) {
		t.Fatal("a crash nobody had reported was refused")
	}
	// A restarted tray looking at the same dead daemon's state file.
	if claimCrashReport(run) {
		t.Fatal("the same run's crash was claimed twice: a tray restart would mail it again")
	}
	// A later run that drew the same PID.
	later := run
	later.startedAt = run.startedAt.Add(3 * time.Hour)
	if !claimCrashReport(later) {
		t.Fatal("a later run that reused the PID was dropped as a duplicate")
	}
}

// TestNoteCrashSkipsARunAlreadyReported: a tray that finds the crash already
// claimed on disk sends nothing — and does not spend the report budget on it,
// or a genuinely new crash in the next hour would be throttled away.
func TestNoteCrashSkipsARunAlreadyReported(t *testing.T) {
	isolatePaths(t)
	run := agentStatus{crashed: true, pid: 22856, startedAt: time.Date(2026, 9, 14, 7, 0, 0, 0, time.UTC)}
	claimCrashReport(run) // an earlier tray process reported it

	ui := &trayUI{}
	ui.noteCrash(run, time.Now())

	if ui.reportedCrashPID != run.pid {
		t.Errorf("reportedCrashPID = %d, want %d: the crash must still be recorded as seen", ui.reportedCrashPID, run.pid)
	}
	if !ui.crashes.lastReport.IsZero() {
		t.Error("a crash that was not sent consumed the report budget")
	}
}
