package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

	// Resolve BEFORE HOME is redirected: when this has to build, the toolchain
	// resolves GOPATH and the module cache from it, and a fake home would make
	// `go build` re-download every dependency into a temp dir it then cannot
	// fully remove.
	bin := resolveUnarrForE2E(t)

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
		t.Fatalf("the real CLI did not yield the startup log — the crash would be lost:\n%s\n%s",
			tail(body, 2000), diagnoseCLI(t, dataDir))
	}
	if !strings.Contains(body, "[cleanup] nothing to clean") {
		t.Fatalf("the real CLI did not yield the daemon log:\n%s\n%s",
			tail(body, 2000), diagnoseCLI(t, dataDir))
	}
}

// diagnoseCLI reports what each half of the collection actually did. The
// assembled body deliberately throws away the reason a source was skipped, so
// without this a failure here says only "no logs available" and leaves whoever
// reads it — on a machine they may not have — with nothing to go on.
func diagnoseCLI(t *testing.T, dataDir string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "--- diagnosis ---\nunarrBin()=%q  dataDir=%s\n", unarrBin(), dataDir)
	for _, name := range []string{"unarr.log", "unarr.boot.log"} {
		if st, err := os.Stat(filepath.Join(dataDir, name)); err == nil {
			fmt.Fprintf(&b, "%s: %d bytes\n", name, st.Size())
		} else {
			fmt.Fprintf(&b, "%s: %v\n", name, err)
		}
	}
	for _, argv := range [][]string{{"daemon", "logs"}, {"daemon", "logs", "--boot"}, {"version"}} {
		start := time.Now()
		out, err := runUnarrOutput(argv...)
		fmt.Fprintf(&b, "%v -> err=%v, %d bytes, %s\n  %s\n",
			argv, err, len(out), time.Since(start).Round(time.Millisecond), tail(strings.TrimSpace(string(out)), 300))
	}
	return b.String()
}

// resolveUnarrForE2E finds the daemon binary this tray shells out to: built
// from THIS tree when a toolchain is around, and only otherwise a deployed one.
//
// The order matters in both directions. Building first means the test measures
// the branch under test rather than whatever `unarr` the developer happens to
// have installed — an older one on $PATH would answer "unknown flag: --boot"
// and turn a real failure into a skip. Falling back to a deployed binary is
// what lets this run on the real-Windows harness, the platform the bug lives
// on: `go test -c` ships a test binary to a guest with no Go toolchain, and a
// build-only helper could never do more than skip exactly where it matters.
func resolveUnarrForE2E(t *testing.T) string {
	t.Helper()
	name := "unarr"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, "github.com/Unarr-app/unarr-cli/cmd/unarr")
	if combined, err := cmd.CombinedOutput(); err == nil {
		return out
	} else {
		t.Logf("no toolchain here (%v: %s) — looking for a deployed CLI", err, firstLine(string(combined)))
	}
	// Sibling of the test binary: how the Windows harness deploys unarr.exe and
	// desktop_test.exe into the same share.
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), name)
		if _, statErr := os.Stat(cand); statErr == nil {
			t.Logf("using the CLI next to this test binary: %s", cand)
			return cand
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		t.Logf("using the CLI on PATH: %s", p)
		return p
	}
	t.Skip("no CLI to test against: cannot build one and none is deployed")
	return ""
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
