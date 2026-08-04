package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withClaim claims a log file and guarantees the process-wide claim is dropped
// again, so one test cannot stamp its ownership into another's state writes.
func withClaim(t *testing.T, path string) {
	t.Helper()
	ClaimLogFile(path, "9.9.9")
	t.Cleanup(ReleaseLogFile)
}

// TestClaimPublishesOwnershipWithoutWaitingForRegistration: the Writer takes
// the file before the daemon has an agent identity, and an external rotator
// that runs in that window (an installer, `unarr self-update`) has to see the
// claim. Registration needs the network; a parked daemon may never get there.
func TestClaimPublishesOwnershipWithoutWaitingForRegistration(t *testing.T) {
	redirectStateDir(t)
	path := filepath.Join(t.TempDir(), "unarr.log")

	withClaim(t, path)

	st := ReadState()
	if st == nil {
		t.Fatal("no state file: an unregistered daemon publishes no ownership at all")
	}
	if st.LogFile != path {
		t.Fatalf("state claims %q, want %q", st.LogFile, path)
	}
	if st.PID != os.Getpid() || st.Status != StatusStarting {
		t.Fatalf("state is %+v, want this PID with status %q", st, StatusStarting)
	}
}

// TestClaimSurvivesAFullStateOverwrite: Register rebuilds DaemonState from
// scratch and writes it. If the claim lived in that struct it would be dropped
// the moment the daemon registered — i.e. exactly when the daemon starts being
// worth protecting.
func TestClaimSurvivesAFullStateOverwrite(t *testing.T) {
	redirectStateDir(t)
	path := filepath.Join(t.TempDir(), "unarr.log")
	withClaim(t, path)

	WriteState(&DaemonState{Status: "running", PID: os.Getpid(), LastHeartbeat: time.Now()})

	st := ReadState()
	if st == nil || st.LogFile != path {
		t.Fatalf("state is %+v, want the claim on %s to survive", st, path)
	}
}

// TestReleaseClearsTheClaim: once the Writer is closed this process owns
// nothing, and no straggler write may say otherwise — a claim that outlived its
// owner would block rotation of a file nobody is holding.
func TestReleaseClearsTheClaim(t *testing.T) {
	redirectStateDir(t)
	ClaimLogFile(filepath.Join(t.TempDir(), "unarr.log"), "9.9.9")
	ReleaseLogFile()

	WriteState(&DaemonState{Status: "running", PID: os.Getpid()})
	if st := ReadState(); st == nil || st.LogFile != "" {
		t.Fatalf("state is %+v, want no claim after ReleaseLogFile", st)
	}
}

// TestClaimDoesNotStealAnotherLiveDaemonsState: a dev agent and the production
// agent share one data dir on purpose (only the lock is config-scoped), so a
// claim that overwrote whatever state it found would point `unarr stop` at the
// wrong process. Publishing waits for registration in that case.
func TestClaimDoesNotStealAnotherLiveDaemonsState(t *testing.T) {
	redirectStateDir(t)
	// A live PID that is not ours: the parent of this test process.
	other := DaemonState{Status: "running", PID: os.Getppid(), Version: "1.2.3"}
	b, err := json.Marshal(other)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(StateFilePath(), b, 0o600); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	if !IsProcessAlive(other.PID) {
		t.Skip("the parent process is gone; nothing to protect in this run")
	}

	withClaim(t, filepath.Join(t.TempDir(), "unarr.log"))

	st := ReadState()
	if st == nil || st.PID != other.PID || st.Version != "1.2.3" {
		t.Fatalf("state is %+v, want the other daemon's record untouched", st)
	}
}
