package main

import (
	"testing"
	"time"
)

// TestCrashIsReportedOncePerRun: the on-disk verdict survives the process that
// made it, and is keyed by run rather than by PID.
func TestCrashIsReportedOncePerRun(t *testing.T) {
	isolatePaths(t)
	run := agentStatus{crashed: true, pid: 14112, startedAt: time.Date(2026, 9, 14, 13, 0, 0, 0, time.UTC)}

	if crashAlreadyReported(run) {
		t.Fatal("a crash nobody had reported reads as reported")
	}
	recordCrashReport(run)
	// A restarted tray looking at the same dead daemon's state file.
	if !crashAlreadyReported(run) {
		t.Fatal("the recorded crash was forgotten: a tray restart would mail it again")
	}
	// A later run that drew the same PID.
	later := run
	later.startedAt = run.startedAt.Add(3 * time.Hour)
	if crashAlreadyReported(later) {
		t.Fatal("a later run that reused the PID was dropped as a duplicate")
	}
}

// TestNoteCrashSkipsARunAlreadyReported: a tray that finds the crash already
// reported on disk sends nothing — and does not spend the report budget on it,
// or a genuinely new crash in the next hour would be throttled away.
func TestNoteCrashSkipsARunAlreadyReported(t *testing.T) {
	isolatePaths(t)
	run := agentStatus{crashed: true, pid: 22856, startedAt: time.Date(2026, 9, 14, 7, 0, 0, 0, time.UTC)}
	recordCrashReport(run) // an earlier tray process reported it

	ui := &trayUI{}
	ui.noteCrash(run, time.Now())

	if ui.reportedCrashPID != run.pid {
		t.Errorf("reportedCrashPID = %d, want %d: the crash must still be recorded as seen", ui.reportedCrashPID, run.pid)
	}
	if !ui.crashes.lastReport.IsZero() {
		t.Error("a crash that was not sent consumed the report budget")
	}
}

// TestThrottledCrashLeavesNoMark: a crash the restart-loop throttle holds back
// was never sent, so it must not read as reported to the next tray process.
func TestThrottledCrashLeavesNoMark(t *testing.T) {
	isolatePaths(t)
	now := time.Now()
	run := agentStatus{crashed: true, pid: 31337, startedAt: now.Add(-time.Minute)}

	ui := &trayUI{}
	ui.crashes.lastReport = now.Add(-time.Minute) // a report just went out for an earlier crash
	ui.noteCrash(run, now)

	if crashAlreadyReported(run) {
		t.Error("a throttled crash was marked as reported, yet nothing was sent")
	}
}

// TestFailedCrashReportIsForgotten: a send that failed takes its mark back, and
// only its own — a marker naming another run is not touched.
func TestFailedCrashReportIsForgotten(t *testing.T) {
	isolatePaths(t)
	run := agentStatus{crashed: true, pid: 4040, startedAt: time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)}
	other := run
	other.startedAt = run.startedAt.Add(time.Hour)

	recordCrashReport(other)
	forgetCrashReport(run)
	if !crashAlreadyReported(other) {
		t.Fatal("forgetting one run's report erased another run's marker")
	}

	recordCrashReport(run)
	forgetCrashReport(run)
	if crashAlreadyReported(run) {
		t.Error("a failed send kept its mark: the next tray would never retry it")
	}
}
