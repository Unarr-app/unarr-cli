package cmd

import (
	"os"
	"strings"
	"testing"
)

// The stop-intent marker is a two-sided contract that no single unit test can
// cover: one side is written by Go here, the other is read by VBScript running
// under wscript.exe after this process is gone. buildLauncherVBS's tests pin the
// reader. These pin the writers — the call sites whose quiet removal would put
// the shim back to guessing, with the worst failure (a paused agent resurrected
// every minute, or a revoked credential restart-looping) landing only on a real
// Windows box.
//
// Source-level on purpose: `unarr stop` terminates a live PID and the revoked
// path lives inside the daemon's main loop, so neither is reachable from a unit
// test. The assertions are about wiring, not behaviour, and are worth exactly
// that much.

func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// TestStopIsNotDecidedByTheStateFile pins the ordering that makes `unarr stop`
// reliable, and the reason it has to be that way.
//
// Stop used to read the state file first and branch on it. That file is written
// during registration — seconds into startup — and SURVIVES a crash, so between
// the shim relaunching a crashed daemon and the new one registering it still
// names the previous, dead PID. Stop then took the "already dead" path, killed
// nothing, recorded nothing, and a live daemon carried on running (measured on
// real Windows). The two steps that do not depend on that file being correct —
// recording the intent, and ending the supervisor — must therefore come FIRST
// and unconditionally; the PID kill is the best-effort extra afterwards.
func TestStopIsNotDecidedByTheStateFile(t *testing.T) {
	src := readSource(t, "daemon_stop.go")
	body := src[strings.Index(src, "func stopDaemonByPID()"):]
	body = body[:strings.Index(body, "\nfunc ")]

	intent := strings.Index(body, "agent.WriteStopIntent()")
	supervisor := strings.Index(body, "stopSupervisor()")
	loadState := strings.Index(body, "agent.LoadState()")
	kill := strings.Index(body, "killPID(state.PID)")

	for name, idx := range map[string]int{
		"agent.WriteStopIntent()": intent,
		"stopSupervisor()":        supervisor,
		"agent.LoadState()":       loadState,
		"killPID(state.PID)":      kill,
	} {
		if idx < 0 {
			t.Fatalf("stopDaemonByPID no longer calls %s; this guard needs updating", name)
		}
	}
	if intent > loadState {
		t.Error("the stop intent must be recorded before the state file is even read — " +
			"a stale file must not be able to skip it")
	}
	if supervisor > loadState {
		t.Error("the supervisor must be stopped before the state file is read — " +
			"ending the task is what stops a daemon the file does not know about")
	}
	if intent > kill {
		t.Error("the stop intent must be recorded BEFORE the kill: taskkill /f leaves no window to record it after")
	}
	// Recorded once, unconditionally. Branch-local copies are how the old version
	// managed to miss a path.
	if n := strings.Count(body, "agent.WriteStopIntent()"); n != 1 {
		t.Errorf("stop intent recorded %d times; want exactly one unconditional call", n)
	}
}

// TestStopSupervisorIsPlatformSplit: the escape hatch is Windows-only. On Linux
// and macOS `unarr stop` already routes through the service manager, and running
// schtasks there would be nonsense.
func TestStopSupervisorIsPlatformSplit(t *testing.T) {
	win := readSource(t, "supervisor_stop_windows.go")
	other := readSource(t, "supervisor_stop_other.go")

	if !strings.Contains(win, "//go:build windows") {
		t.Error("supervisor_stop_windows.go is missing its build tag")
	}
	if !strings.Contains(other, "//go:build !windows") {
		t.Error("supervisor_stop_other.go is missing its build tag")
	}
	if !strings.Contains(win, `"/end"`) || !strings.Contains(win, `"unarr"`) {
		t.Errorf("the Windows stop no longer ends the scheduled task:\n%s", win)
	}
	// It must stay best-effort: a foreground daemon has no task, and schtasks
	// erroring there is not a failure of "stop".
	if !strings.Contains(win, "_ = cmd.Run()") {
		t.Errorf("ending the task must be best-effort (no task / not installed is fine):\n%s", win)
	}
}

// TestUninstallRecordsIntent: uninstall taskkills the daemon while its scheduled
// task still exists, so without the marker the shim reports a failure the task
// can still act on.
func TestUninstallRecordsIntent(t *testing.T) {
	src := readSource(t, "daemon_install.go")
	if !strings.Contains(src, "agent.WriteStopIntent()") {
		t.Error("uninstall no longer records the stop intent before taskkill")
	}
	if !strings.Contains(src, "reapStateAfterExit(state.PID)") {
		t.Error("uninstall no longer reaps the state file — the tray would read it as a crash")
	}
}

// TestDaemonConsumesAndRecordsIntent covers the daemon's own two obligations:
// clear the marker when it starts (so it cannot suppress the respawn after a
// LATER crash), and set it on the deliberate revoked-credential exit (so the
// supervisor does not restart-loop against a 410 only the user can fix).
func TestDaemonConsumesAndRecordsIntent(t *testing.T) {
	src := readSource(t, "daemon.go")

	clear := strings.Index(src, "agent.ClearStopIntent()")
	if clear < 0 {
		t.Fatal("the daemon no longer clears the stop intent at startup")
	}
	// It has to be cleared early — before anything that can fail — or a marker
	// left over from a previous stop outlives its purpose.
	if lock := strings.Index(src, "instanceLock.TryLock()"); lock >= 0 && clear > lock {
		t.Error("clear the stop intent before the startup work that can fail, not after")
	}
	if !strings.Contains(src, "agent.WriteStopIntent()") {
		t.Error("no deliberate-exit path records the stop intent (revoked credential / signal shutdown)")
	}
}

// TestDaemonStartReapsStateOutsideADefer is the false-crash-report fix, and its
// subtlety: the reap must NOT be deferred. A panic has to unwind past it and
// leave the state file behind, because that is the crash the report exists for.
func TestDaemonStartReapsStateOutsideADefer(t *testing.T) {
	src := readSource(t, "daemon_exit.go")

	idx := strings.Index(src, "agent.ReapOwnState()")
	if idx < 0 {
		t.Fatal("runDaemon no longer reaps the state file — clean exits still look like crashes")
	}
	// Look at the statement before it: a `defer` here would silently start
	// suppressing genuine panic crash reports.
	head := src[:idx]
	lastLine := head[strings.LastIndex(head, "\n")+1:]
	if strings.Contains(lastLine, "defer") {
		t.Error("ReapOwnState must not be deferred: a panic would then reap the state file and hide a real crash")
	}
	// The reap has to sit AFTER the daemon returns, not before it runs.
	if call := strings.Index(src, "runDaemonStart()"); call < 0 || call > idx {
		t.Error("runDaemon must call runDaemonStart and reap afterwards")
	}
	// And the seal has to come BEFORE the reap, or a goroutine still unwinding
	// can write the state file straight back after it is removed.
	seal := strings.Index(src, "agent.SealState()")
	if seal < 0 {
		t.Fatal("runDaemon no longer seals the state file — a late writer can resurrect it")
	}
	if seal > idx {
		t.Error("SealState must precede ReapOwnState, else the reap can be undone by a straggler")
	}
}

// TestEveryDaemonEntryPointReapsState: `unarr start` and `unarr up` both bring
// the daemon up, and a call that skips the wrapper is a silent regression — that
// path simply goes back to mailing crash reports for clean exits.
func TestEveryDaemonEntryPointReapsState(t *testing.T) {
	for _, f := range []string{"daemon.go", "up.go"} {
		src := readSource(t, f)
		if !strings.Contains(src, "return runDaemon()") {
			t.Errorf("%s does not start the daemon through runDaemon() — its exits are unguarded", f)
		}
		// Guard the easy mistake: calling the inner function directly from a
		// command, bypassing the reap.
		for _, l := range strings.Split(src, "\n") {
			if strings.Contains(l, "return runDaemonStart()") {
				t.Errorf("%s calls runDaemonStart() directly, bypassing the reap: %q", f, strings.TrimSpace(l))
			}
		}
	}
}
