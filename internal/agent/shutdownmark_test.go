package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMarkShuttingDownBeatsTheDrain is the whole point of the mark: a stop that
// is killed BEFORE it can remove the state file must still leave something the
// tray can tell apart from a crash.
//
// The tray's rule (cmd/unarr-desktop.readStatus) is "state file says running +
// PID is gone ⇒ crashed ⇒ mail a report". The shutdown path drains downloads
// for up to 30s before it removes the file, while Windows grants a process ~5s
// at shutdown and `taskkill /T` grants none — so the file that survives is the
// one written here, not the removal that never ran.
func TestMarkShuttingDownBeatsTheDrain(t *testing.T) {
	tmpDir := t.TempDir()
	orig := stateFilePathFn
	stateFilePathFn = func() string { return filepath.Join(tmpDir, "daemon.state.json") }
	t.Cleanup(func() { stateFilePathFn = orig })

	d := &Daemon{State: DaemonState{
		AgentID:   "agent-1",
		Status:    "running",
		PID:       os.Getpid(),
		StartedAt: time.Now(),
	}}
	WriteState(&d.State)

	d.MarkShuttingDown()
	// …and then the supervisor kills us here: no Deregister, no RemoveState.

	persisted, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState after MarkShuttingDown: %v", err)
	}
	if persisted.Status == "running" {
		t.Fatal("state file still says running — the tray would read this as a crash and mail a report for a stop the user asked for")
	}
	if persisted.Status != "shutting_down" {
		t.Fatalf("Status = %q, want %q", persisted.Status, "shutting_down")
	}
}

// TestMarkShuttingDownUnderConcurrentHeartbeats is the -race guard. The mark is
// set on the SIGNAL goroutine while the sync loop is still running and writing
// the same struct from its own; before mutateState that was an unsynchronised
// write to State.Status, and the heartbeat racing it could marshal "running"
// straight back over the mark — restoring the crash signature the mark exists
// to remove.
func TestMarkShuttingDownUnderConcurrentHeartbeats(t *testing.T) {
	tmpDir := t.TempDir()
	orig := stateFilePathFn
	stateFilePathFn = func() string { return filepath.Join(tmpDir, "daemon.state.json") }
	t.Cleanup(func() { stateFilePathFn = orig })

	d := &Daemon{State: DaemonState{AgentID: "agent-1", Status: "running", PID: os.Getpid(), StartedAt: time.Now()}}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { // stands in for the sync loop's OnSyncAttempt/OnSyncSuccess
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				d.mutateState(func(st *DaemonState) { st.LastAlive = time.Now() })
				_ = d.stateStatus()
			}
		}
	}()

	time.Sleep(5 * time.Millisecond)
	d.MarkShuttingDown()
	time.Sleep(5 * time.Millisecond) // let the heartbeats keep going after the mark
	close(stop)
	<-done

	persisted, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if persisted.Status != "shutting_down" {
		t.Fatalf("Status = %q after concurrent heartbeats, want the mark to have survived them", persisted.Status)
	}
}

// TestMarkShuttingDownSticksAcrossLaterWrites: the sync loop keeps running
// during the drain and rewrites the state file on every attempt (see
// Daemon.Run's OnSyncAttempt). The mark lives on d.State rather than being a
// one-off write to disk precisely so those later writes carry it too — a mark
// that the next heartbeat reverted to "running" would be worthless.
func TestMarkShuttingDownSticksAcrossLaterWrites(t *testing.T) {
	tmpDir := t.TempDir()
	orig := stateFilePathFn
	stateFilePathFn = func() string { return filepath.Join(tmpDir, "daemon.state.json") }
	t.Cleanup(func() { stateFilePathFn = orig })

	d := &Daemon{State: DaemonState{AgentID: "agent-1", Status: "running", PID: os.Getpid(), StartedAt: time.Now()}}
	d.MarkShuttingDown()

	// What OnSyncAttempt does mid-drain.
	d.State.LastAlive = time.Now()
	WriteState(&d.State)

	persisted, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if persisted.Status != "shutting_down" {
		t.Fatalf("Status = %q after a mid-drain heartbeat, want it to survive as %q", persisted.Status, "shutting_down")
	}
}
