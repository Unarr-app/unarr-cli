//go:build !windows

package main

import (
	"os/exec"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// TestRealKillStillReadsAsACrash guards the other side of the false-positive
// fixes: a daemon that really dies uncleanly — SIGKILL, OOM killer, a panic all
// leave the same "running + PID gone" file — must keep being reported. The state
// file layout is byte-identical in v1.11.5 and HEAD (git diff v1.11.5 HEAD --
// internal/agent/state.go is empty), so an older daemon is judged the same way.
func TestRealKillStillReadsAsACrash(t *testing.T) {
	isolatePaths(t)
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("no sleep binary: %v", err)
	}
	now := time.Now()
	writeStateFile(t, agent.DaemonState{
		AgentID: "killed", Status: "running", Version: "1.11.5", PID: cmd.Process.Pid,
		StartedAt: now.Add(-21 * time.Second), LastHeartbeat: now, LastAlive: now,
	})
	if s := readStatus(); !s.running || s.crashed {
		t.Fatalf("a live daemon is running, not crashed: %+v", s)
	}

	cmd.Process.Kill()
	cmd.Wait()

	s := readStatus()
	if !s.crashed || s.pid != cmd.Process.Pid {
		t.Fatalf("SIGKILLed daemon with a \"running\" state file must be reported as a crash: %+v", s)
	}
}

// TestShuttingDownMarkIsNotACrash: the mark MarkShuttingDown writes first on any
// stop signal means a supervisor that kills the drain leaves a deliberate stop.
func TestShuttingDownMarkIsNotACrash(t *testing.T) {
	isolatePaths(t)
	now := time.Now()
	writeStateFile(t, agent.DaemonState{
		AgentID: "stopping", Status: "shutting_down", Version: "1.11.5", PID: deadPID,
		StartedAt: now.Add(-time.Minute), LastHeartbeat: now, LastAlive: now,
	})
	if s := readStatus(); s.crashed {
		t.Fatalf("a shutting_down state file is a deliberate stop: %+v", s)
	}
}
