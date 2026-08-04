package cmd

import (
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/logging"
)

// daemonLogOwner reports the live daemon that owns path and rotates it from the
// inside, so an EXTERNAL rotation can refuse instead of copy-truncating under
// it. Wired into every Options an outsider rotates with (see logRingOptions).
//
// It answers from what the daemon SAID — the log file it claimed in the state
// file — never from a filesystem probe, which cannot see a Go owner at all.
//
// Two ways it must say "no owner", both load-bearing:
//
//   - The state file names a different file (a foreground `unarr start` logs to
//     stdout and claims nothing; the boot log belongs to the supervisor).
//   - The record is STALE. isDaemonAlive is the CLI's one definition of a live
//     daemon — a PID the OS still has, with a heartbeat that has not gone
//     silent — and reusing it is what keeps a crashed daemon's leftover state
//     from blocking rotation of that log forever.
func daemonLogOwner(path string) (logging.Owner, bool) {
	st := agent.ReadState()
	if st == nil || st.LogFile == "" || !sameLogFile(st.LogFile, path) {
		return logging.Owner{}, false
	}
	if !isDaemonAlive(st) {
		return logging.Owner{}, false // stale state file: nobody is holding it
	}
	return logging.Owner{PID: st.PID, What: "the running unarr daemon"}, true
}

// sameLogFile compares two log paths the way the filesystem would: cleaned, and
// case-insensitively on Windows, where the launcher's `\` spelling and a
// config-derived one differ only in case surprisingly often.
func sameLogFile(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
