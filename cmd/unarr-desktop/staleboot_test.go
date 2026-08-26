package main

import (
	"os"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/sysinfo"
)

// deadPID is the repo's stand-in for a process that is gone (see
// agent.TestIsProcessAliveBogus): far above any PID an OS hands out.
const deadPID = 0x7FFFFFFE

// requireBootTime skips a test on a platform (or a sandbox) with no boot-time
// source, where the whole mechanism deliberately does nothing.
func requireBootTime(t *testing.T) {
	t.Helper()
	if _, ok := sysinfo.BootTime(); !ok {
		t.Skip("no boot-time source here — readStatus keeps its pre-boot-check behaviour")
	}
}

// beforeAnyBoot is safely older than the boot of any machine running these
// tests, so the real platform source is exercised rather than a fake.
var beforeAnyBoot = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// TestReadStatusReapsAStateFileFromThePreviousBoot is the crash-report false
// positive: a Windows box that reboots for updates in its maintenance window
// kills the daemon before its 30s drain can seal and remove the state file,
// leaving "running + PID gone" — byte for byte what a panic leaves. The tray
// mailed a crash report for a restart nobody would call a crash.
func TestReadStatusReapsAStateFileFromThePreviousBoot(t *testing.T) {
	isolatePaths(t)
	requireBootTime(t)
	writeStateFile(t, agent.DaemonState{
		AgentID:       "rebooted",
		Status:        "running",
		PID:           deadPID,
		StartedAt:     beforeAnyBoot,
		LastHeartbeat: beforeAnyBoot,
	})

	s := readStatus()
	if s.crashed {
		t.Fatal("a state file older than the boot is a reboot leftover, not a crash")
	}
	if s.running {
		t.Fatal("nothing is running: the state file predates this boot")
	}
	if _, err := os.Stat(agent.StateFilePath()); !os.IsNotExist(err) {
		t.Fatal("the stale state file must be reaped, or the tray re-decides it every tick")
	}
}

// TestReadStatusIgnoresAReusedPIDFromThePreviousBoot: after a reboot the
// recorded PID can belong to an unrelated process — here, this very test
// binary. IsProcessAlive says yes, and the tray used to render a healthy green
// agent that was not running at all. The boot check has to run BEFORE the
// liveness check for this to hold.
func TestReadStatusIgnoresAReusedPIDFromThePreviousBoot(t *testing.T) {
	isolatePaths(t)
	requireBootTime(t)
	writeStateFile(t, agent.DaemonState{
		AgentID:       "pid-reused",
		Status:        "running",
		PID:           os.Getpid(), // very much alive, and not the daemon
		StartedAt:     beforeAnyBoot,
		LastHeartbeat: beforeAnyBoot,
	})

	if s := readStatus(); s.running {
		t.Fatal("a live PID does not resurrect a state file written before this boot")
	}
}

// TestReadStatusReapsAPreBootStateWhateverItsStatus: the status field records
// what the daemon was DOING, not when the file was written. An "upgrading" or
// "shutting_down" relic of the previous boot is just as stale, and leaving it
// on disk would keep the tray reasoning about a daemon that no longer exists.
func TestReadStatusReapsAPreBootStateWhateverItsStatus(t *testing.T) {
	for _, status := range []string{"running", "upgrading", "shutting_down"} {
		t.Run(status, func(t *testing.T) {
			isolatePaths(t)
			requireBootTime(t)
			writeStateFile(t, agent.DaemonState{
				AgentID:       "relic",
				Status:        status,
				PID:           deadPID,
				StartedAt:     beforeAnyBoot,
				LastHeartbeat: beforeAnyBoot,
			})

			s := readStatus()
			if s.crashed || s.running {
				t.Fatalf("pre-boot state with status %q must read as stopped, got %+v", status, s)
			}
			if _, err := os.Stat(agent.StateFilePath()); !os.IsNotExist(err) {
				t.Fatalf("pre-boot state with status %q must be reaped", status)
			}
		})
	}
}

// TestReadStatusStillReportsARealCrash is the regression guard the tests above
// are worth nothing without: a daemon that lived and died inside THIS boot is
// exactly what the crash report exists for.
func TestReadStatusStillReportsARealCrash(t *testing.T) {
	isolatePaths(t)
	now := time.Now()
	writeStateFile(t, agent.DaemonState{
		AgentID:       "died-for-real",
		Status:        "running",
		Version:       "1.8.1",
		PID:           deadPID,
		StartedAt:     now.Add(-20 * time.Minute),
		LastHeartbeat: now.Add(-1 * time.Minute),
		// LastAlive is what dates the file (agent.LastAliveAt takes the newest
		// stamp), and it is `now` on purpose. A fixture whose newest stamp is
		// minutes old is a state file that could predate the host's OWN last
		// shutdown — which agent.StateFromPreviousBoot now reads — and on a box
		// booted moments ago this case would flip to "reboot leftover" and stop
		// testing anything. A daemon that just died leaves a stamp from just now.
		LastAlive: now,
	})

	s := readStatus()
	if !s.crashed {
		t.Fatal("state file from this boot + PID gone is a crash and must still be reported")
	}
	if s.pid != deadPID || s.version != "1.8.1" {
		t.Fatalf("crash status lost the metadata a report needs: %+v", s)
	}
	if _, err := os.Stat(agent.StateFilePath()); err != nil {
		t.Fatal("a real crash must KEEP its state file: reaping it would erase the evidence")
	}
}

// TestReadStatusOfflineDaemonStaysRunning is the false-reap this could most
// plausibly have caused: LastHeartbeat only advances on a successful sync, so a
// daemon that has been up for hours with no network carries an ancient one.
// Judged by the heartbeat alone, the tray would reap a LIVE daemon's state file
// and show the agent as stopped.
func TestReadStatusOfflineDaemonStaysRunning(t *testing.T) {
	isolatePaths(t)
	requireBootTime(t)
	writeStateFile(t, agent.DaemonState{
		AgentID:       "offline-but-alive",
		Status:        "running",
		PID:           os.Getpid(),
		StartedAt:     time.Now().Add(-30 * time.Second), // this boot
		LastHeartbeat: beforeAnyBoot,                     // sync has never succeeded
		LastAlive:     time.Now(),                        // ...but it is trying, right now
	})

	s := readStatus()
	if !s.running {
		t.Fatal("a live daemon whose sync is failing must still read as running")
	}
	if _, err := os.Stat(agent.StateFilePath()); err != nil {
		t.Fatal("a live daemon's state file must not be reaped")
	}
}

// TestReadStatusRunningDaemonIsUntouched: the ordinary happy path must not
// change — a live daemon with current timestamps stays running, and its state
// file stays on disk.
func TestReadStatusRunningDaemonIsUntouched(t *testing.T) {
	isolatePaths(t)
	writeState(t, "healthy")

	s := readStatus()
	if !s.running || s.crashed {
		t.Fatalf("a live daemon must read as running: %+v", s)
	}
	if _, err := os.Stat(agent.StateFilePath()); err != nil {
		t.Fatal("a running daemon's state file must not be reaped")
	}
}
