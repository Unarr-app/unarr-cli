// Package service answers the one question the CLI and the tray both need
// before stopping the daemon: is it running under a supervisor that will
// respawn it?
//
// Killing the PID of a systemd `Restart=always` unit (or a launchd KeepAlive
// agent) is not a stop — the supervisor brings the daemon back RestartSec
// later. That is what users saw as "I pause it from the tray and it turns
// itself back on a few seconds later": the tray ran `unarr stop`, which
// signalled the PID, and systemd respawned it 10s afterwards. Anything that
// means "stay stopped" has to go through the service manager instead.
package service

import (
	"os"
	"path/filepath"
	"runtime"
)

// SystemdUnitName is the systemd user unit and the Windows scheduled task name.
const SystemdUnitName = "unarr"

// LaunchdLabel is the launchd agent label.
const LaunchdLabel = "com.torrentclaw.unarr"

// UnitPath is the systemd user unit `unarr daemon install` writes.
// Empty when the home directory cannot be resolved.
func UnitPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return SystemdUnitPathIn(home)
}

// SystemdUnitPathIn is UnitPath for an explicit home — install/uninstall
// already have one resolved and must write exactly the path detection reads.
func SystemdUnitPathIn(home string) string {
	return filepath.Join(home, ".config", "systemd", "user", SystemdUnitName+".service")
}

// PlistPath is the launchd user agent `unarr daemon install` writes.
func PlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist")
}

// Respawns reports whether an installed supervisor would restart the daemon
// after it exits, making a PID-level stop useless.
//
// Detection is by the artifact on disk — the same file install writes and
// uninstall removes — so it costs no subprocess and is still correct while the
// service is stopped. Windows is deliberately false: the Task Scheduler task
// runs at logon and never respawns on exit, so stopping by PID is a real stop
// there.
func Respawns() bool {
	var path string
	switch runtime.GOOS {
	case "linux":
		path = UnitPath()
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		path = PlistPath(home)
	default:
		return false
	}
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
