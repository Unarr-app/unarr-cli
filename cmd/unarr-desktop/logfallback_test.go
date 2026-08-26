package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dataDirWithLogs isolates config/data paths for this test and returns the
// data dir the fallback will read, created and empty. Reuses the package's
// existing isolatePaths/unarrDataDir rather than a second copy of the same
// per-platform env dance.
func dataDirWithLogs(t *testing.T) string {
	t.Helper()
	isolatePaths(t)
	dir := unarrDataDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	return dir
}

// TestFallbackRecoversLogsWhenTheCLICannotRun is the regression test for the
// windows/amd64 field report whose ENTIRE log tail was
//
//	COULD NOT READ THE LOGS: running `unarr daemon logs` failed
//	(exit status 0xc0000142)
//
// Both log reads exec the same binary, so a binary that will not start took the
// daemon log AND the boot log — the file holding the panic — down with it, and
// the crash report carried no evidence about the crash at all.
//
// With the fallback, an unexecutable CLI costs nothing: the files are still on
// disk and both sections come back.
func TestFallbackRecoversLogsWhenTheCLICannotRun(t *testing.T) {
	dataDir := dataDirWithLogs(t)
	writeLog(t, filepath.Join(dataDir, "unarr.log"), "daemon-line-from-disk\n")
	writeLog(t, filepath.Join(dataDir, "unarr.boot.log"),
		"panic: runtime error: index out of range\n")

	// Every invocation fails the way the loader failure did.
	restore := runUnarrOutput
	t.Cleanup(func() { runUnarrOutput = restore })
	runUnarrOutput = func(_ ...string) ([]byte, error) {
		return nil, errors.New("exit status 0xc0000142")
	}

	body := string(collectReportLogs())

	if !strings.Contains(body, "daemon-line-from-disk") {
		t.Fatalf("daemon log not recovered from disk:\n%s", body)
	}
	// The whole point: the panic must reach the report.
	if !strings.Contains(body, "panic: runtime error") {
		t.Fatalf("boot log (where panics land) not recovered from disk:\n%s", body)
	}
	// The reader must still learn the CLI was broken, not just get the logs.
	if !strings.Contains(body, "0xc0000142") {
		t.Fatalf("the report must still say why the CLI read failed:\n%s", body)
	}
	if strings.Contains(body, "COULD NOT READ THE LOGS") {
		t.Fatalf("logs WERE read; the failure placeholder must not appear:\n%s", body)
	}
}

// TestFallbackStaysSilentWithNoFiles: the fallback recovers evidence, it does
// not invent it. With no files on disk the body must be byte for byte the
// placeholder it always was.
func TestFallbackStaysSilentWithNoFiles(t *testing.T) {
	dataDirWithLogs(t) // exists, but empty

	stubUnarr(t, map[string]reply{})

	body := string(collectReportLogs())
	if !strings.Contains(body, "COULD NOT READ THE LOGS") {
		t.Fatalf("with nothing on disk the failure placeholder must survive:\n%s", body)
	}
	if strings.Contains(body, "read directly from") {
		t.Fatalf("nothing was read; the fallback must not claim it was:\n%s", body)
	}
}

// TestFallbackPrefersTheCLI: the CLI stays the primary source. It knows where
// its own logs live — on a systemd install they are in the journal and there is
// no file to read — so a working CLI must never be second-guessed by a stale
// file sitting next to it.
func TestFallbackPrefersTheCLI(t *testing.T) {
	dataDir := dataDirWithLogs(t)
	writeLog(t, filepath.Join(dataDir, "unarr.log"), "STALE-FILE-CONTENT\n")

	stubUnarr(t, map[string]reply{"daemon logs": {out: "live-from-cli\n"}})

	body := string(collectReportLogs())
	if !strings.Contains(body, "live-from-cli") {
		t.Fatalf("the CLI's answer must be used when it works:\n%s", body)
	}
	if strings.Contains(body, "STALE-FILE-CONTENT") {
		t.Fatalf("a working CLI must not be overridden by the file on disk:\n%s", body)
	}
}

// TestFallbackReadsTheTailOfALargeLog: rotation is opt-in and off by default,
// so these files are routinely far larger than the budget. The fallback must
// bound what it reads AND keep the END, which is where a crash is.
func TestFallbackReadsTheTailOfALargeLog(t *testing.T) {
	dataDir := dataDirWithLogs(t)
	big := strings.Repeat("filler-line-that-is-old\n", (fallbackLogBudget/24)+5000)
	writeLog(t, filepath.Join(dataDir, "unarr.log"), big+"THE-LAST-LINE\n")

	out, ok := fallbackDaemonLog()
	if !ok {
		t.Fatal("large log not read")
	}
	if len(out) > fallbackLogBudget {
		t.Fatalf("read %d bytes, budget is %d", len(out), fallbackLogBudget)
	}
	if !strings.Contains(string(out), "THE-LAST-LINE") {
		t.Fatal("the fallback kept the head instead of the tail; a crash is at the END")
	}
}

// TestFallbackNoteWithoutAnError: "the CLI ran and printed nothing" is a
// different story from "the CLI would not start", and must not be reported as
// "failed (<nil>)".
func TestFallbackNoteWithoutAnError(t *testing.T) {
	note := string(fallbackNote(nil, []byte("body\n")))
	if strings.Contains(note, "<nil>") {
		t.Fatalf("a nil error must not be printed as a failure reason: %q", note)
	}
	if !strings.Contains(note, "printed nothing") {
		t.Fatalf("a successful-but-empty read must say so: %q", note)
	}
}
