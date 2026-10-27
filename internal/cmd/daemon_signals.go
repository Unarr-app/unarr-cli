package cmd

import (
	"os"
	"os/signal"
	"syscall"
)

// daemonStopSignals are the signals that mean "stop, on purpose". SIGHUP is one
// of them: a daemon started in a terminal (`unarr start`, ssh) gets it when the
// terminal closes, and Go's default for it is to die on the spot — no
// MarkShuttingDown, no reap — leaving "status: running + PID gone", byte for
// byte the crash signature unarr-desktop mails a report for. Closing the window
// is a stop, not a crash.
//
// Skipped when SIGHUP was inherited as ignored (`nohup unarr start`): Notify
// would silently re-arm it and turn a daemon that was meant to outlive its
// terminal into one that quits with it.
func daemonStopSignals() []os.Signal {
	sigs := []os.Signal{syscall.SIGINT, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return sigs
}

// survivePipeClose stops a closed stdout/stderr from killing the daemon.
//
// Go terminates a process that writes to a broken pipe on fd 1 or 2 unless it
// calls Notify for SIGPIPE. The tray starts a bare `unarr start` with both
// descriptors on a pipe it owns (startUnarr), so when the tray exits or is
// restarted the daemon's next log line killed it — silently, with the state
// file still saying "running" — and the next tray reported a crash for a daemon
// the tray itself had orphaned. With the signal swallowed the write just fails
// (log and fmt ignore it) and the daemon keeps downloading.
//
// Notify, not signal.Ignore: an ignored disposition is inherited across exec,
// and ffmpeg and friends should keep SIGPIPE's default.
func survivePipeClose() {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
}
