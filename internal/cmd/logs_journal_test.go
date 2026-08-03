package cmd

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/logging"
)

// hasArg reports whether flag appears in args, with value as the next element.
func hasArg(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestJournalArgsAlwaysTargetTheUnitInCatFormat(t *testing.T) {
	args := journalArgs(logging.Query{Lines: 50}, false)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--user -u unarr") {
		t.Fatalf("args %v do not target the user unit", args)
	}
	// -o cat strips the "host unarr[pid]:" prefix so the parser sees the same
	// line shape unarr.log holds.
	if !hasArg(args, "-o", "cat") {
		t.Fatalf("args %v must ask for the bare message", args)
	}
	if !hasArg(args, "-n", "50") {
		t.Fatalf("args %v must carry the requested line count", args)
	}
}

func TestJournalArgsKeepAHugeLineCountParseableByJournalctl(t *testing.T) {
	// Regression: `unarr logs -n 1099511627776` handed journalctl a number its
	// -n cannot parse, so it retried it as a match and died with "Failed to add
	// match ... Invalid argument" instead of showing the log.
	args := journalArgs(logging.Query{Lines: 1099511627776}, false)
	if !hasArg(args, "-n", "all") {
		t.Fatalf("args %v must degrade an unparseable count to -n all", args)
	}
	// A count journalctl does handle is still passed through verbatim.
	if args := journalArgs(logging.Query{Lines: 4000}, false); !hasArg(args, "-n", "4000") {
		t.Fatalf("args %v must pass a sane count through unchanged", args)
	}
}

func TestJournalArgsPushSinceDownToTheJournal(t *testing.T) {
	cut := time.Date(2025, time.January, 20, 9, 30, 0, 0, time.Local)
	args := journalArgs(logging.Query{Lines: 10, Since: cut}, false)
	if !hasArg(args, "--since", "2025-01-20 09:30:00") {
		t.Fatalf("args %v must let the journal do the time seek — it indexes by time, we do not", args)
	}
}

func TestJournalArgsFollowDropsThePager(t *testing.T) {
	args := journalArgs(logging.Query{Lines: 10}, true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-f") {
		t.Fatalf("args %v are missing -f", args)
	}
	if strings.Contains(joined, "--no-pager") || strings.Contains(joined, " -n ") {
		t.Fatalf("args %v: -f already streams; a pager/limit would fight it", args)
	}
}

// startNeverEndingProcess spawns a stand-in for `journalctl -f`: a child that
// will not exit on its own, so a bare Wait() on it is a hang.
func startNeverEndingProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("journalctl is a systemd-only path")
	}
	cmd := exec.Command("sleep", "600")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a test child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

func TestFinishJournalKillsTheChildWhenTheFilterFailed(t *testing.T) {
	// Regression: the old code waited unconditionally, so a filter that gave up
	// without draining the pipe (a bad --grep under -f) blocked forever on a
	// `journalctl -f` that had no reason to exit.
	cmd := startNeverEndingProcess(t)
	want := errors.New("invalid --grep pattern")

	done := make(chan error, 1)
	go func() { done <- finishJournal(cmd, want, true) }()

	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Fatalf("got %v, want the filter's own error surfaced", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("finishJournal waited on a process that never exits")
	}
}

func TestFinishJournalReportsAFailedOneShotButNotAnInterruptedFollow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("journalctl is a systemd-only path")
	}
	newFailed := func() *exec.Cmd {
		cmd := exec.Command("false")
		if err := cmd.Start(); err != nil {
			t.Skipf("cannot spawn a test child: %v", err)
		}
		return cmd
	}
	if err := finishJournal(newFailed(), nil, false); err == nil {
		t.Fatal("a one-shot journalctl that failed must be reported")
	}
	// Under -f the user's Ctrl-C reaches journalctl too — a normal end.
	if err := finishJournal(newFailed(), nil, true); err != nil {
		t.Fatalf("got %v, want a follow's non-zero exit treated as a normal end", err)
	}
}

func TestJournalWindowWidensOnlyWhenSomethingFilters(t *testing.T) {
	if got := journalWindow(logging.Query{Lines: 50}); got != 50 {
		t.Fatalf("got %d, want exactly what was asked for when nothing filters", got)
	}
	if got := journalWindow(logging.Query{Lines: 50, Grep: "nzb"}); got <= 50 {
		t.Fatalf("got %d — a grep needs a wider scan or -n returns too few matches", got)
	}
	if got := journalWindow(logging.Query{Lines: 50, MinLevel: logging.LevelWarn}); got <= 50 {
		t.Fatalf("got %d — a level filter needs a wider scan too", got)
	}
	if got := journalWindow(logging.Query{Lines: 100000, Grep: "x"}); got != journalScanCap {
		t.Fatalf("got %d, want the scan capped at %d", got, journalScanCap)
	}
	if got := journalWindow(logging.Query{}); got != logging.DefaultLines {
		t.Fatalf("got %d, want the default line count", got)
	}
	// An absurd -n must not overflow the widening into a negative window, which
	// would hand journalctl a nonsense "-n -N".
	if got := journalWindow(logging.Query{Lines: 1 << 62, Grep: "x"}); got != journalScanCap {
		t.Fatalf("got %d, want the scan capped at %d", got, journalScanCap)
	}
}
