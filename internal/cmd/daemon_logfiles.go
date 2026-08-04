package cmd

import (
	"context"
	"path/filepath"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/logging"
)

// errLogFileName is the second log file a macOS agent can have. Installs made
// by a build whose plist pointed StandardErrorPath at its own file keep writing
// here for as long as that plist lives — and since the daemon logs through
// log.Printf (stderr), that file is the one that actually accumulates. Nothing
// rewrites the plist until the next `unarr daemon install`, so the janitor has
// to bound this file too, or the 24/7 install those size limits exist for still
// fills its disk.
const errLogFileName = "unarr.err.log"

const (
	// bootLogFileName is the file the SUPERVISOR holds: launchd's Standard*Path,
	// cmd.exe's `>>` in the Windows shim, the *os.File a detached parent hands
	// its child. It exists because the things that diagnose a failed start —
	// the "unarr Daemon" banner, cobra's fatal error print, a Go panic dump —
	// all bypass log.SetOutput and go straight to fd 1/2. It must NOT be the
	// file the daemon owns: on Windows cmd.exe grants only FILE_SHARE_READ, so
	// pointing both at unarr.log would make the daemon's own FILE_APPEND_DATA
	// open fail and leave it with no log at all.
	bootLogFileName = "unarr.boot.log"
	// bootLogMaxSizeMB / bootLogMaxFiles are the boot log's budget ONCE ROTATION
	// IS ON. The size is FIXED, deliberately not the user's [daemon]
	// log_max_size_mb: this file holds banners and stack traces, and someone who
	// raised the main log to 500 MB did not ask for a 500 MB banner file.
	//
	// What log_max_size_mb DOES decide for this file is whether it is bounded at
	// all. Rotation is opt-in and off by default (see config.DaemonConfig), and
	// "off" has to mean off for every ring the daemon owns — a boot log quietly
	// rotating on its own would leave exactly the class of failure the descope
	// removed, one file to the left. See bootLogRingOptions.
	bootLogMaxSizeMB = 2
	bootLogMaxFiles  = 1
	// bootLogMaxBytes is the same budget in bytes, for the one rotator that
	// cannot go through logging.Options: the VBScript shim, which size-checks
	// the file itself because nothing in Go can bound a file cmd.exe holds.
	bootLogMaxBytes = bootLogMaxSizeMB * 1024 * 1024
)

// daemonBootLogPath returns the supervisor-held startup log.
func daemonBootLogPath() string { return filepath.Join(config.DataDir(), bootLogFileName) }

// rotationEnabled reports whether the user opted into log rotation at all.
//
// [daemon] log_max_size_mb is 0 by default, which means every rotation path in
// this package — the daemon's own Writer, the copy-truncate janitor, the
// installers' pre-launch trim, `unarr logs rotate`, the boot log's ring and the
// VBScript shim's trim — is a no-op until it is set. One predicate rather than
// a `> 0` scattered per call site, so "is rotation on?" has a single answer.
func rotationEnabled() bool { return loadConfig().Daemon.LogMaxSizeMB > 0 }

// bootLogRingOptions is the boot log's own ring policy: one rotated slot at a
// fixed budget, independent of the SIZE the main log is configured with — but
// gated on the same opt-in switch, so a default install rotates nothing at all.
// A zero MaxSizeMB is what logging.Options already spells "rotation disabled",
// so this needs no second mechanism.
func bootLogRingOptions(path string) logging.Options {
	if !rotationEnabled() {
		return logging.Options{Path: path, MaxFiles: bootLogMaxFiles}
	}
	return logging.Options{Path: path, MaxSizeMB: bootLogMaxSizeMB, MaxFiles: bootLogMaxFiles}
}

// bootLogTrimBytes is the budget the VBScript shim size-checks the boot log
// against, in bytes, or 0 to emit no trim at all.
//
// Resolved at INSTALL time and baked into the generated script, because nothing
// running inside wscript.exe can read unarr's config. Turning rotation on later
// therefore needs one `unarr daemon install` before the shim's trim comes back
// — documented in README's Logs section, since a silently-unbounded boot log is
// exactly the kind of surprise this descope is meant to stop shipping.
func bootLogTrimBytes() int64 {
	if !rotationEnabled() {
		return 0
	}
	return bootLogMaxBytes
}

// rotateBootLogIn trims the boot log under dir before a supervisor opens it and
// holds it for the whole session (launchd's Standard*Path, the Windows task).
// Same gap, and same best-effort contract, as rotateDaemonLogIn.
func rotateBootLogIn(dir string) {
	_ = logging.RotateNow(bootLogRingOptions(filepath.Join(dir, bootLogFileName)))
}

// daemonLogPaths lists every log file the daemon supervises, live file first.
// Single source of truth for the janitor: `clean` sweeps the same names (plus
// their rotated copies), so a file one of them knows about and the other does
// not is a file that grows without a ceiling.
func daemonLogPaths() []string {
	return []string{
		daemonLogPath(),
		filepath.Join(config.DataDir(), errLogFileName),
		daemonBootLogPath(),
	}
}

// logJanitorOptions is the ring policy for one supervised path. The boot log
// has its own fixed budget; everything else follows [daemon] log_max_size_mb.
func logJanitorOptions(path string) logging.Options {
	if filepath.Base(path) == bootLogFileName {
		return bootLogRingOptions(path)
	}
	return logRingOptions(path)
}

// startLogJanitors runs one copy-truncate janitor per FOREIGN-HELD daemon log
// for as long as ctx lives. A path that does not exist on this platform simply
// never rotates — RotateNow treats a missing file as nothing to do, which is
// what makes this safe to start unconditionally.
//
// owned is the log THIS process holds a Writer on (see installDaemonLogWriter),
// or "" when the supervisor still owns the redirect. It is skipped: the Writer
// rotates that file by rename, and a second rotator on the same path would
// copy-truncate underneath it — two mechanisms fighting over one file, which is
// exactly the ring corruption this whole design exists to remove.
func startLogJanitors(ctx context.Context, every time.Duration, owned string) {
	for _, path := range daemonLogPaths() {
		if owned != "" && filepath.Clean(path) == filepath.Clean(owned) {
			continue
		}
		go logging.Sweep(ctx, logJanitorOptions(path), every)
	}
}
