package agent

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func writeStateAt(t *testing.T, pid int) {
	t.Helper()
	// The seal is one-way in a real process; tests share one, so lift it.
	resetStateSeal()
	t.Cleanup(resetStateSeal)
	WriteState(&DaemonState{
		AgentID:   "agent-reap",
		Status:    "running",
		Version:   "1.8.1",
		PID:       pid,
		StartedAt: time.Now(),
	})
	if ReadState() == nil {
		t.Fatal("setup: state file was not written")
	}
}

// TestReapOwnStateRemovesOurs is the false-crash-report fix: a deliberate exit
// must not leave behind the "status running + PID gone" pair that unarr-desktop
// reads as a crash and mails a report for.
func TestReapOwnStateRemovesOurs(t *testing.T) {
	redirectStateDir(t)
	writeStateAt(t, os.Getpid())

	ReapOwnState()

	if ReadState() != nil {
		t.Error("our own state file survived the reap — a clean exit still looks like a crash")
	}
}

// TestReapOwnStateKeepsAnotherDaemons is the guard that makes the reap safe to
// call unconditionally. A dev agent (UNARR_CONFIG_DIR) and the production agent
// share one data dir on purpose, so an unconditional remove would let either
// delete the other's live state and make a healthy daemon look stopped.
func TestReapOwnStateKeepsAnotherDaemons(t *testing.T) {
	redirectStateDir(t)
	other := os.Getpid() + 1
	writeStateAt(t, other)

	ReapOwnState()

	st := ReadState()
	if st == nil {
		t.Fatal("reaped another daemon's state file")
	}
	if st.PID != other {
		t.Errorf("state PID = %d, want the other daemon's %d", st.PID, other)
	}
}

// TestReapOwnStateNoFileIsSafe: the early-exit paths (no API key, bad config)
// return before any state is written, so the reap runs against nothing.
func TestReapOwnStateNoFileIsSafe(t *testing.T) {
	dir := redirectStateDir(t)
	if _, err := os.Stat(filepath.Join(dir, "daemon.state.json")); !os.IsNotExist(err) {
		t.Fatalf("setup: expected no state file, got %v", err)
	}
	ReapOwnState() // must not panic
	if ReadState() != nil {
		t.Error("reap invented a state file")
	}
}

// TestReapOwnStateIsIdempotent: the signal path already removes the state via
// Deregister, and the wrapper reaps again on the way out. The second call must
// be a no-op rather than an error.
func TestReapOwnStateIsIdempotent(t *testing.T) {
	redirectStateDir(t)
	writeStateAt(t, os.Getpid())

	ReapOwnState()
	ReapOwnState()

	if ReadState() != nil {
		t.Error("state file reappeared")
	}
}

// TestSealStateBlocksLateWriters is the other half of the clean-stop fix, and
// the one a live SIGTERM actually exposed: the daemon logged "Agent
// deregistered" and "Daemon stopped.", removed its state file — and a goroutine
// still unwinding wrote it straight back, "status": "running" and all. The tray
// then reads that resurrected file as a crash and mails a report for a stop the
// user asked for.
func TestSealStateBlocksLateWriters(t *testing.T) {
	redirectStateDir(t)
	writeStateAt(t, os.Getpid())

	SealState()
	t.Cleanup(resetStateSeal)
	ReapOwnState()

	// A straggler from the sync loop / a task reporter, arriving after shutdown.
	WriteState(&DaemonState{AgentID: "late", Status: "running", PID: os.Getpid()})

	if st := ReadState(); st != nil {
		t.Errorf("a late writer resurrected the state file (%+v) — the stop still looks like a crash", st)
	}
}

// TestSealStateIsOrderIndependent: the seal must hold even if the straggler
// lands between the removal and the seal, which is the real race — hence
// sealing first in cmd.runDaemon.
func TestSealStateIsOrderIndependent(t *testing.T) {
	redirectStateDir(t)
	writeStateAt(t, os.Getpid())

	ReapOwnState()
	WriteState(&DaemonState{AgentID: "racer", Status: "running", PID: os.Getpid()}) // slips in
	SealState()
	t.Cleanup(resetStateSeal)
	ReapOwnState() // the seal-then-reap pairing sweeps it up for good

	if ReadState() != nil {
		t.Error("state file survived the seal-and-reap pairing")
	}
}

// TestSealStateBeatsWritersAlreadyInFlight is the one a plain flag could not
// pass, and the reason SealState holds the write mutex rather than just setting
// an atomic. A writer that has ALREADY passed a flag check still has a MkdirAll,
// a WriteFile and a Rename to go, so its rename lands after the seal and after
// the removal — which is exactly what a live SIGTERM shutdown did.
func TestSealStateBeatsWritersAlreadyInFlight(t *testing.T) {
	redirectStateDir(t)
	resetStateSeal()
	t.Cleanup(resetStateSeal)

	// Writers hammering the state file, like the sync loop and task reporters
	// unwinding through a shutdown.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					WriteState(&DaemonState{AgentID: "racer", Status: "running", PID: os.Getpid()})
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond) // let them get going

	SealState()
	ReapOwnState()

	// From here nothing may recreate the file, even with writers still spinning.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if st := ReadState(); st != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("a writer resurrected the state file after the seal: %+v", st)
		}
	}
	close(stop)
	wg.Wait()

	if st := ReadState(); st != nil {
		t.Errorf("state file present after the writers stopped: %+v", st)
	}
}

// TestWriteStateWorksBeforeTheSeal guards the obvious regression: the seal must
// not break normal operation, only shutdown.
func TestWriteStateWorksBeforeTheSeal(t *testing.T) {
	redirectStateDir(t)
	writeStateAt(t, os.Getpid())

	if ReadState() == nil {
		t.Fatal("WriteState is a no-op before any seal — the daemon would never report state")
	}
}
