package main

// Whether a crash the tray has just seen gets reported.
//
// The in-memory guards (reportedCrashPID, the crashTracker throttle) only know
// what THIS process has seen. A tray that is restarted — by the user, by its
// own self-update, at the next login — finds the same dead daemon's state file
// still on disk and would mail the same crash again. So the verdict is also
// recorded on disk, next to the state file it is about.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

const crashMarkerName = "desktop.crash-reported"

func crashMarkerPath() string {
	return filepath.Join(filepath.Dir(agent.StateFilePath()), crashMarkerName)
}

// noteCrash records a crash and decides whether it is reported. Called from
// renderDaemonStatus, under renderMu.
func (ui *trayUI) noteCrash(s agentStatus, now time.Time) {
	if s.pid == ui.reportedCrashPID || !now.After(ui.suppressCrashUntil) {
		return
	}
	ui.reportedCrashPID = s.pid
	ui.crashes.observe(now)
	if !claimCrashReport(s) {
		return
	}
	// A restart loop re-reports one failure: the developers need it once, not
	// once per restart.
	if ui.crashes.shouldReport(now) {
		go handleCrash(s)
	}
}

// crashKey identifies one daemon RUN. A PID alone does not: PIDs are recycled,
// so a later crash that drew the PID of one already reported would be dropped.
// StartedAt is stamped once per run.
func crashKey(s agentStatus) string {
	return fmt.Sprintf("%d %s", s.pid, s.startedAt.UTC().Format(time.RFC3339Nano))
}

// claimCrashReport reports whether this run's crash has not been reported yet,
// and records that it now has been.
//
// Best-effort on the write: a marker that cannot be written costs at worst a
// duplicate report, never a lost one — which is the direction to fail in.
func claimCrashReport(s agentStatus) bool {
	key := crashKey(s)
	path := crashMarkerPath()
	if prev, err := os.ReadFile(path); err == nil && string(prev) == key {
		return false
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(key), 0o600)
	return true
}
