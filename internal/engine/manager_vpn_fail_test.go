package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// TestPauseForVPN_FallsBackToFailWhenPauseRejected drives the uncovered fail-safe
// branch of pauseForVPN: it normally reports a mid-download VPN drop as PAUSED
// (StatusCancelled, resumable), but if the state machine REJECTS that transition
// (the task is in a state it cannot be cancelled from) it must fall back to fail()
// so the task reaches a terminal state instead of sticking non-terminal forever —
// a VPN drop at the wrong moment must never hang the task.
func TestPauseForVPN_FallsBackToFailWhenPauseRejected(t *testing.T) {
	// nil client → local-only reporter, so ReportFinal is a hermetic no-op (no net).
	reporter := NewProgressReporter(nil, time.Hour)
	mgr := NewManager(
		ManagerConfig{MaxConcurrent: 1, OutputDir: t.TempDir()},
		reporter,
		&vpnDropDownloader{method: MethodTorrent},
	)
	p := newFakePersister()
	mgr.SetTaskStore(p)

	// Drive a task into StatusVerifying: validTransitions[Verifying] =
	// {Organizing, Failed, Resolving} — StatusCancelled is NOT allowed (so the pause
	// is rejected), but StatusFailed IS (so the fail() fallback succeeds).
	task := NewTaskFromAgent(dlTask("pause-reject"))
	for _, to := range []TaskStatus{StatusResolving, StatusDownloading, StatusVerifying} {
		if err := task.Transition(to); err != nil {
			t.Fatalf("setup transition -> %s: %v", to, err)
		}
	}
	p.Add(agent.Task{ID: task.ID}) // pretend it is in the resume store

	mgr.pauseForVPN(context.Background(), task)

	// It must be TERMINAL (Failed), never stuck in the non-terminal Verifying.
	if got := task.GetStatus(); got != StatusFailed {
		t.Errorf("status = %q, want %q (fail() fallback)", got, StatusFailed)
	}
	task.mu.RLock()
	msg := task.ErrorMessage
	task.mu.RUnlock()
	if !strings.HasPrefix(msg, "VPN tunnel down:") {
		t.Errorf("error message = %q, want it to start with 'VPN tunnel down:'", msg)
	}

	// The fail() fallback is a GENUINE terminal, so — unlike the resumable-pause
	// happy path — it drops the resume-store entry.
	if p.has(task.ID) {
		t.Error("resume-store entry kept after the fail() fallback; a genuine terminal must drop it")
	}

	// And it is reported as a final "failed" state so the web learns it stopped.
	var found bool
	for _, s := range mgr.TaskStates() {
		if s.TaskID == task.ID {
			found = true
			if s.Status != "failed" {
				t.Errorf("reported status = %q, want failed", s.Status)
			}
		}
	}
	if !found {
		t.Error("no final state recorded — the web would never learn the task stopped")
	}
}
