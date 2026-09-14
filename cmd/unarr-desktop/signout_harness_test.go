package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/sysinfo"
)

// TestHarnessSignOutIsNotACrash runs ONLY from the logon probe that
// test/windows/smoke-logoff-arm.ps1 arms — plain `go test` skips it. The probe
// copies the state file a real sign-out left behind and runs this in the session
// that follows, before the scheduled task relaunches the daemon and overwrites
// the evidence. It feeds that file through the tray's own readStatus and requires
// the verdict "not a crash": the report the next sign-in used to mail.
func TestHarnessSignOutIsNotACrash(t *testing.T) {
	src := os.Getenv("UNARR_HARNESS_SIGNOUT_STATE")
	if src == "" {
		t.Skip("harness-only: UNARR_HARNESS_SIGNOUT_STATE names a state file a sign-out left behind")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read the sign-out state file: %v", err)
	}
	isolatePaths(t)
	if err := os.MkdirAll(filepath.Dir(agent.StateFilePath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent.StateFilePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}

	st := agent.ReadState()
	if st == nil {
		t.Fatalf("the sign-out state file does not parse:\n%s", data)
	}
	logon, ok := sysinfo.SessionLogonTime()
	t.Logf("state: pid %d status %q started %v last alive %v; session logon %v (known: %v)",
		st.PID, st.Status, st.StartedAt, agent.LastAliveAt(st), logon, ok)
	if agent.IsProcessAlive(st.PID) {
		t.Skipf("pid %d is alive in this session (reused) - the probe ran too late to judge", st.PID)
	}
	if !agent.StateFromPreviousLogon(st) {
		t.Errorf("a state file last written %v was not judged older than this session's logon %v",
			agent.LastAliveAt(st), logon)
	}
	if s := readStatus(); s.crashed {
		t.Error("readStatus reads the sign-out as a crash: the tray would mail a crash report for a logoff")
	}
}
