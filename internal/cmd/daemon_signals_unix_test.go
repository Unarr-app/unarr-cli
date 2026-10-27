//go:build !windows

package cmd

import (
	"bufio"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A real private process stands in for the daemon's signal handling: it installs
// exactly what runDaemon does (survivePipeClose + daemonStopSignals), keeps
// logging to stdout like the daemon, and on a stop signal records which one in
// UNARR_TEST_SIG_RECEIPT and exits 0 — the "clean shutdown" branch.
func init() {
	if os.Getenv("UNARR_TEST_SIG_ROLE") != "daemon" {
		return
	}
	survivePipeClose()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, daemonStopSignals()...)
	os.Stdout.WriteString("ready\n")
	tick := time.NewTicker(20 * time.Millisecond)
	for {
		select {
		case sig := <-sigCh:
			os.WriteFile(os.Getenv("UNARR_TEST_SIG_RECEIPT"), []byte(sig.String()), 0o600)
			os.Exit(0)
		case <-tick.C:
			os.Stdout.WriteString("log line\n") // EPIPE once the reader is gone
		}
	}
}

// startSignalDaemon launches the stand-in with its stdout on a pipe the test
// owns, and returns once it reports ready.
func startSignalDaemon(t *testing.T) (*exec.Cmd, *os.File, string) {
	t.Helper()
	receipt := t.TempDir() + "/receipt"
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "UNARR_TEST_SIG_ROLE=daemon", "UNARR_TEST_SIG_RECEIPT="+receipt)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	if line, _ := bufio.NewReader(out).ReadString('\n'); strings.TrimSpace(line) != "ready" {
		t.Fatalf("stand-in daemon never became ready: %q", line)
	}
	return cmd, out.(*os.File), receipt
}

func waitCleanStop(t *testing.T, cmd *exec.Cmd, receipt, want string) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon died instead of taking the clean-stop path: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
	got, _ := os.ReadFile(receipt)
	if string(got) != want {
		t.Fatalf("stop signal seen = %q, want %q", got, want)
	}
}

// TestDaemonSurvivesItsOutputPipeClosing is the tray-restart false positive: the
// tray owns the daemon's stdout/stderr pipe (startUnarr), exits, and the daemon's
// next log line used to kill it with SIGPIPE — state file still "running", PID
// gone, i.e. a mailed crash report for a daemon the tray itself orphaned.
func TestDaemonSurvivesItsOutputPipeClosing(t *testing.T) {
	cmd, out, receipt := startSignalDaemon(t)
	out.Close()                        // the tray went away
	time.Sleep(300 * time.Millisecond) // many log lines into the broken pipe
	// No liveness probe here: signal 0 answers nil for an unreaped zombie. A
	// daemon killed by SIGPIPE shows up as a non-nil Wait error below.
	cmd.Process.Signal(syscall.SIGTERM)
	waitCleanStop(t, cmd, receipt, "terminated")
}

// TestDaemonTreatsHangupAsAStop: closing the terminal a foreground daemon runs in
// sends SIGHUP; it must take the same clean path as SIGTERM, not die mid-run.
func TestDaemonTreatsHangupAsAStop(t *testing.T) {
	if signal.Ignored(syscall.SIGHUP) {
		t.Skip("SIGHUP is ignored here (nohup runner) and the stand-in inherits it")
	}
	cmd, _, receipt := startSignalDaemon(t)
	cmd.Process.Signal(syscall.SIGHUP)
	waitCleanStop(t, cmd, receipt, "hangup")
}

// TestDaemonStopSignalsRespectNohup: a SIGHUP inherited as ignored (nohup) is a
// request to outlive the terminal; Notify would silently undo it.
func TestDaemonStopSignalsRespectNohup(t *testing.T) {
	has := func(sigs []os.Signal) bool {
		for _, s := range sigs {
			if s == syscall.SIGHUP {
				return true
			}
		}
		return false
	}
	if !has(stopSignals(false)) {
		t.Fatal("SIGHUP must be a stop signal by default")
	}
	if has(stopSignals(true)) {
		t.Fatal("SIGHUP ignored at start (nohup) must stay ignored")
	}
}
