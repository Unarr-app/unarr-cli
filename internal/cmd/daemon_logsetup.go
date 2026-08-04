package cmd

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/logging"
	"github.com/spf13/cobra"
)

// daemonLogFileFlag is `unarr start --log-file`: the path this daemon OWNS for
// this run. Empty means "nobody owns it here" — the supervisor's redirect is
// still the log, and rotation stays with the copy-truncate janitor.
//
// A per-invocation flag rather than an env var or an isatty guess, for three
// reasons the platform matrix forces:
//   - a stray UNARR_LOG_FILE in a shell profile would silently mute an
//     interactive `unarr start`;
//   - a container has no tty either, so "not a tty ⇒ supervised" would make the
//     Docker daemon own a file inside the container and hide its output from
//     `docker logs`, the only tool that reads it there;
//   - the flag is self-documenting in the scheduled task's action, in the
//     launchd plist and in ps/Task Manager.
var daemonLogFileFlag string

// bindDaemonLogFlag registers --log-file on a command that runs the daemon in
// the foreground. Only the supervised launchers pass it; a human running
// `unarr start` gets today's behaviour byte for byte.
func bindDaemonLogFlag(cmd *cobra.Command) {
	cmd.Flags().StringVar(&daemonLogFileFlag, "log-file", "",
		"own this log file and rotate it in place (set by the service launchers; leave unset to log to stdout/stderr)")
}

// ownedLogPath is the log file this run owns, cleaned, or "" when it owns none.
// The janitor consults it so it never sweeps a file a Writer is rotating.
func ownedLogPath() string {
	if daemonLogFileFlag == "" {
		return ""
	}
	return filepath.Clean(daemonLogFileFlag)
}

// installDaemonLogWriter points log.Printf at the file this daemon owns and
// returns the closer that releases it.
//
// ORDERING IS LOAD-BEARING: call it AFTER the single-instance flock is taken
// and BEFORE the banner. A second daemon that loses the flock race must not
// rename the live log of the one already running, and everything that happens
// before the lock (config errors, lock contention) belongs on stderr — which
// under every supervised launcher IS the boot log.
//
// A Writer that cannot be opened (AV lock, read-only dir) is NOT fatal: one
// line on stderr and logging carries on there. A log file that cannot be opened
// must never stop downloads.
func installDaemonLogWriter() func() {
	if daemonLogFileFlag == "" {
		return func() {}
	}
	w, err := logging.NewWriter(logRingOptions(daemonLogFileFlag))
	if err != nil {
		fmt.Fprintf(os.Stderr, "unarr: %v - logging to stderr instead\n", err)
		return func() {}
	}
	// Announce the ownership the moment it is real, and only then: every
	// external rotator (`unarr logs rotate`, the installers' pre-launch trim,
	// `unarr self-update`) reads this claim and stands down. A rotation that
	// asked the FILESYSTEM instead got "sure, go ahead" while this Writer was
	// mid-file — Go grants FILE_SHARE_WRITE on Windows and locks nothing on
	// POSIX — which is how an upgrade came to truncate a live daemon's log.
	agent.ClaimLogFile(w.Path(), Version)
	log.SetOutput(w)
	// Run marker. The banner now lands in the supervisor's boot log, so the
	// owned log needs its own unambiguous delimiter: once rotation is by rename
	// there is otherwise no clue in the file where one run ends and the next
	// begins.
	log.Printf("[daemon] unarr %s starting (pid %d)", Version, os.Getpid())
	return func() {
		log.SetOutput(os.Stderr)
		agent.ReleaseLogFile()
		_ = w.Close()
	}
}
