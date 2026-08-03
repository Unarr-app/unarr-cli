package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"

	"github.com/Unarr-app/unarr-cli/internal/logging"
	"github.com/Unarr-app/unarr-cli/internal/service"
	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// journalScanFactor widens the journal window when a content filter is active:
// the matching lines are a subset, so asking for exactly -n would return fewer
// than requested. Bounded by journalScanCap because a busy unit's journal can
// be enormous and `unarr logs` must stay instant.
// journalMaxLines is the largest count journalctl's -n will parse: it reads the
// value as a signed 32-bit int, and anything above that stops looking like a
// number to it and is retried as a match expression — "Failed to add match
// '4294967295': Invalid argument".
const (
	journalScanFactor = 20
	journalScanCap    = 5000
	journalMaxLines   = 1<<31 - 1
)

// runJournalLogs reads the systemd journal and applies the SAME filters the
// file reader applies.
//
// journalctl has a --grep and a -p of its own, but neither means what ours do:
// its patterns are PCRE (ours are RE2) and its priorities come from the unit,
// which logs everything on stdout at one level. Piping its output through the
// one filter is what makes --level / --grep / --since behave identically on a
// systemd box and on a NAS reading unarr.log.
func runJournalLogs(q logging.Query, follow bool) error {
	return journalTo(os.Stdout, q, follow)
}

// journalTo is runJournalLogs with the destination as a parameter, so
// `unarr support-bundle` can capture the same output into a buffer instead of
// growing a second, subtly different journalctl invocation of its own.
func journalTo(w io.Writer, q logging.Query, follow bool) error {
	cmd := exec.Command("journalctl", journalArgs(q, follow)...)
	winproc.HideWindow(cmd)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("read journal: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("run journalctl: %w", err)
	}
	return finishJournal(cmd, filterJournal(w, out, q, follow), follow)
}

// filterJournal pipes journalctl's output through the query's filter.
func filterJournal(w io.Writer, r io.Reader, q logging.Query, follow bool) error {
	if follow {
		return logging.FilterLive(r, q, w)
	}
	return logging.FilterTail(r, q, w)
}

// finishJournal reaps the journalctl child now that the filter is done.
//
// A filter error means the pipe was left undrained, and waiting on that is a
// hang, not a delay: `journalctl -f` never exits on its own, and even the
// one-shot form blocks the moment its output fills the pipe buffer. So kill
// first and reap after — only a filter that ran to completion earns a plain
// Wait. Under -f the user's Ctrl-C reaches journalctl too, which is a normal
// end, so its exit status is reported for the one-shot form only.
func finishJournal(cmd *exec.Cmd, filterErr error, follow bool) error {
	if filterErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return filterErr
	}
	if err := cmd.Wait(); err != nil && !follow {
		return fmt.Errorf("journalctl: %w", err)
	}
	return nil
}

// journalArgs builds the journalctl invocation. `-o cat` strips the syslog-ish
// "host unarr[pid]:" prefix so what comes out is the daemon's own line —
// identical to what unarr.log holds, which is what the parser and the renderer
// expect.
func journalArgs(q logging.Query, follow bool) []string {
	args := []string{"--user", "-u", service.SystemdUnitName, "-o", "cat"}
	if !q.Since.IsZero() {
		// Let the journal do the time seek; it indexes by time and we do not.
		args = append(args, "--since", q.Since.Format("2006-01-02 15:04:05"))
	}
	if follow {
		// -f implies live output; --no-pager would be meaningless here.
		return append(args, "-f")
	}
	return append(args, "--no-pager", "-n", journalLineArg(q))
}

// journalLineArg renders the -n value. A window journalctl cannot parse becomes
// "all", which is what "more lines than the journal could possibly hold" means
// anyway — and -n is only a seek optimisation here, since the tail filter trims
// to the requested count regardless.
func journalLineArg(q logging.Query) string {
	if n := journalWindow(q); n <= journalMaxLines {
		return strconv.Itoa(n)
	}
	return "all"
}

// journalWindow is how many journal lines to hand the filter. Without a content
// filter every line survives, so -n is exactly what was asked for.
func journalWindow(q logging.Query) int {
	want := q.Lines
	if want <= 0 {
		want = logging.DefaultLines
	}
	if q.Grep == "" && q.MinLevel <= logging.LevelInfo {
		return want
	}
	// Cap-first, so a wild -n cannot overflow the multiplication into a negative
	// window and hand journalctl a nonsense "-n -N".
	if want >= journalScanCap/journalScanFactor {
		return journalScanCap
	}
	return want * journalScanFactor
}
