package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The control plane's port and token live in the daemon state file — that file
// is how `unarr downloads` and the desktop tray find it. Register() REBUILDS
// DaemonState from scratch, and a registration that lands after the control
// server bound (retries behind a flaky network, or a recovery from
// sign_in_required — both seen on the first live run of this feature) used to
// wipe both fields, leaving every local action reporting "no control plane"
// against a daemon that had one.
func TestSetControlPlane_SurvivesStateRebuild(t *testing.T) {
	tmpDir := t.TempDir()
	orig := stateFilePathFn
	stateFilePathFn = func() string { return filepath.Join(tmpDir, "daemon.state.json") }
	t.Cleanup(func() { stateFilePathFn = orig })

	d := &Daemon{}
	d.SetControlPlane(45123, "s3cret-token")

	persisted, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState after SetControlPlane: %v", err)
	}
	if persisted.ControlPort != 45123 || persisted.ControlToken != "s3cret-token" {
		t.Fatalf("state file = port %d token %q, want the values just published",
			persisted.ControlPort, persisted.ControlToken)
	}

	// Simulate what Register does: a fresh DaemonState built from scratch,
	// carrying only what the daemon remembers.
	d.State = DaemonState{
		AgentID:      "agent-1",
		Status:       "running",
		PID:          os.Getpid(),
		StartedAt:    time.Now(),
		ControlPort:  d.controlPort,
		ControlToken: d.controlToken,
	}
	WriteState(&d.State)

	after, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState after the rebuild: %v", err)
	}
	if after.ControlPort != 45123 || after.ControlToken != "s3cret-token" {
		t.Fatalf("a state rebuild lost the control plane: port %d token %q",
			after.ControlPort, after.ControlToken)
	}
}

// A daemon that never armed a control plane must publish zero values, so
// clients take the offline path instead of dialling port 0.
func TestDaemonState_NoControlPlaneIsZero(t *testing.T) {
	tmpDir := t.TempDir()
	orig := stateFilePathFn
	stateFilePathFn = func() string { return filepath.Join(tmpDir, "daemon.state.json") }
	t.Cleanup(func() { stateFilePathFn = orig })

	WriteState(&DaemonState{AgentID: "agent-1", Status: "running", PID: os.Getpid()})

	st, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.ControlPort != 0 || st.ControlToken != "" {
		t.Fatalf("expected no control plane, got port %d token %q", st.ControlPort, st.ControlToken)
	}
}
