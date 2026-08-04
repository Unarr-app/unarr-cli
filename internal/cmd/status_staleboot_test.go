package cmd

import (
	"os"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/sysinfo"
)

// beforeAnyBoot is safely older than the boot of any machine running these
// tests, so the real platform boot-time source is exercised.
var beforeAnyBoot = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// TestIsDaemonAliveRejectsAPreBootState is the CLI half of the tray's
// pre-boot check. `unarr status` runs on boxes with no tray at all (a Linux
// server, a Docker host), where nothing else ever reaps a stale state file, so
// after a reboot it would keep answering from the previous boot's record — and
// if that PID had been handed to an unrelated process, answer "running".
func TestIsDaemonAliveRejectsAPreBootState(t *testing.T) {
	if _, ok := sysinfo.BootTime(); !ok {
		t.Skip("no boot-time source here — isDaemonAlive keeps its heartbeat-only behaviour")
	}
	// A live PID (this test process) with pre-boot timestamps: the combination
	// only the boot check can reject. The heartbeat rule would reject it too,
	// but for the wrong reason and only by luck — see the next test.
	state := &agent.DaemonState{
		Status:        "running",
		PID:           os.Getpid(),
		StartedAt:     beforeAnyBoot,
		LastHeartbeat: beforeAnyBoot,
	}
	if isDaemonAlive(state) {
		t.Fatal("a state file written before this boot describes a daemon the machine outlived")
	}
}

// TestIsDaemonAliveAcceptsALiveDaemon guards the other direction: the check
// must not start calling healthy daemons dead. Fresh timestamps, live PID.
func TestIsDaemonAliveAcceptsALiveDaemon(t *testing.T) {
	state := &agent.DaemonState{
		Status:        "running",
		PID:           os.Getpid(),
		StartedAt:     time.Now().Add(-30 * time.Second),
		LastHeartbeat: time.Now(),
	}
	if !isDaemonAlive(state) {
		t.Fatal("a daemon started this boot, heartbeating now, with a live PID, is alive")
	}
}

// TestIsDaemonAliveHeartbeatRuleStillApplies pins the pre-existing rule the
// boot check sits in front of but does not replace: a PID reused WITHIN this
// boot is caught by the stale heartbeat, which the boot instant cannot see.
func TestIsDaemonAliveHeartbeatRuleStillApplies(t *testing.T) {
	state := &agent.DaemonState{
		Status:        "running",
		PID:           os.Getpid(),
		StartedAt:     time.Now().Add(-10 * time.Minute),
		LastHeartbeat: time.Now().Add(-5 * time.Minute), // this boot, but stale
	}
	if isDaemonAlive(state) {
		t.Fatal("a heartbeat five minutes old must still read as not-running")
	}
}
