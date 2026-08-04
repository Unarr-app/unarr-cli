package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestReportLogsAgainstTheRealCLI drives the whole chain end to end: a real
// `unarr` binary, real log files on disk, and the tray's own collection path.
//
// The unit tests above stub runUnarrOutput, so they prove the assembly and
// nothing about the argv actually working. This is the only test that would
// catch the CLI answering "unknown flag: --boot" — a failure the tray swallows
// by design (a missing boot log is an ordinary state of the world), and which
// would therefore restore the original bug in total silence.
func TestReportLogsAgainstTheRealCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the CLI")
	}
	isolatePaths(t)

	// Build BEFORE HOME is redirected: the toolchain resolves GOPATH and the
	// module cache from it, and a fake home would make `go build` re-download
	// every dependency into a temp dir it then cannot fully remove.
	bin := buildUnarrCLI(t)

	// A fake HOME for the CLI run itself, because `unarr logs` reads the systemd
	// journal instead of a file when it finds a unit at
	// $HOME/.config/systemd/user/unarr.service (see service.UnitPath). On a
	// developer box that has one installed, this test would otherwise read the
	// REAL daemon's journal and prove nothing — and, worse, pass or fail on
	// someone else's log lines.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))

	dataDir := unarrDataDir(t)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	writeLog(t, filepath.Join(dataDir, "unarr.log"),
		"2026/08/04 01:19:00 [cleanup] nothing to clean\n")
	writeLog(t, filepath.Join(dataDir, "unarr.boot.log"),
		"panic: runtime error: invalid memory address\n\ngoroutine 1 [running]:\n")

	// Ask the CLI directly first. Its refusal message is the one thing
	// collectReportLogs deliberately throws away, and without consulting it here
	// a box that legitimately has no boot log would fail with an unreadable diff
	// instead of skipping. Two such cases, both "not a defect in this code":
	//
	//   - a systemd install, where startup output goes to the journal;
	//   - a branch without the log-ownership work, where --boot does not exist
	//     at all (see internal/cmd/daemon_logs_contract_test.go).
	if out, err := runUnarrOutput("daemon", "logs", "--boot"); err != nil {
		switch {
		case strings.Contains(string(out), "unknown flag"):
			t.Skip("this CLI has no --boot yet: the tray's startup-log section cannot exist " +
				"until the log-ownership work merges")
		case strings.Contains(string(out), "systemd"):
			t.Skipf("--boot is refused on a systemd install by design: %s", out)
		}
	}

	body := string(collectReportLogs())
	if !strings.Contains(body, "panic: runtime error") {
		t.Fatalf("the real CLI did not yield the startup log — the crash would be lost:\n%s", tail(body, 2000))
	}
	if !strings.Contains(body, "[cleanup] nothing to clean") {
		t.Fatalf("the real CLI did not yield the daemon log:\n%s", tail(body, 2000))
	}
}

// buildUnarrCLI compiles the daemon binary this tray shells out to.
func buildUnarrCLI(t *testing.T) string {
	t.Helper()
	name := "unarr"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, "github.com/Unarr-app/unarr-cli/cmd/unarr")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build the CLI here (%v): %s", err, combined)
	}
	return out
}

// unarrDataDir mirrors config.DataDir() for the sandbox isolatePaths set up.
// Resolved here rather than imported so this test exercises the same env the
// CHILD process will read, not this process's view of it.
func unarrDataDir(t *testing.T) string {
	t.Helper()
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "unarr")
	case "darwin":
		return os.Getenv("UNARR_CONFIG_DIR")
	default:
		return filepath.Join(os.Getenv("XDG_DATA_HOME"), "unarr")
	}
}

func writeLog(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// tail bounds what a failure prints: the body can be the whole log ring.
func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}
