package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/Unarr-app/unarr-cli/internal/logging"
)

// Source selection for `unarr logs --boot`, the SUPERVISOR-held startup log.
//
// The main log is what the daemon writes once it is running; the boot log is
// what it wrote before that — and what a crash wrote on the way out. On Windows
// this is the path that actually needed a command of its own: someone
// diagnosing a daemon that never came up at logon has no shell habit and no
// redirect of their own, and the log directory otherwise looks empty.

// bootLogQuery repoints an already-validated query at the boot ring. The ring is
// its own size, one rotated slot, independent of [daemon] log_max_files.
func bootLogQuery(q logging.Query) logging.Query {
	q.Path = daemonBootLogPath()
	q.MaxFiles = bootLogMaxFiles
	return q
}

// bootSourceUnavailable rejects --boot where there is no such file to read.
//
// On a systemd box the daemon's stdout/stderr go to the journal — the unit has
// no StandardOutput= and installSystemd tells the user to read it with
// journalctl — so the startup output IS the ordinary log there. Silently
// falling back to the journal would be worse than an error: it would answer a
// question about a file that does not exist with content from somewhere else.
func bootSourceUnavailable() error {
	if usesJournald() {
		return errors.New("--boot has no file to read here: this is a systemd install, " +
			"so the daemon's startup output already goes to the journal — read it with 'unarr logs'")
	}
	return nil
}

// missingBootLogError is what --boot reports when the ring is not on disk.
func missingBootLogError(path string) error {
	return fmt.Errorf("no startup log yet at %s — it is written by the service launcher, "+
		"so it appears once the daemon has been started by 'unarr daemon install' or 'unarr start' (detached)", path)
}

// missingDaemonLogError is the "no daemon log yet" dead end, plus the one thing
// that gets the user out of it: an install whose daemon dies before it can open
// its own log leaves an EMPTY-looking data dir and a boot log holding the actual
// reason. Point at it only when there is something in it to read.
func missingDaemonLogError(path string) error {
	msg := fmt.Sprintf("no daemon log yet at %s — start the daemon first ('unarr start' or 'unarr daemon start')", path)
	if bootLogHasContent() {
		msg += fmt.Sprintf("\nSomething WAS written to %s before that — read it with 'unarr logs --boot'", daemonBootLogPath())
	}
	return errors.New(msg)
}

// bootLogHasContent reports whether any slot of the boot ring holds bytes. Size
// matters, not existence: the launchers create the file on every start, so a
// zero-byte one would offer a hint that leads nowhere.
func bootLogHasContent() bool {
	path := daemonBootLogPath()
	for _, p := range append([]string{path}, logging.RotatedPaths(path, bootLogMaxFiles)...) {
		if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
			return true
		}
	}
	return false
}
