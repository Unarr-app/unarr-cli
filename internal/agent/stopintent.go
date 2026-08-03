package agent

import (
	"os"
	"path/filepath"
)

// Deliberate-stop intent marker.
//
// The Windows scheduled task is the only supervisor the agent has there (no
// systemd Restart=always, no launchd KeepAlive), and it decides whether to
// respawn from ONE signal: the exit code of the launcher shim. That leaves the
// shim needing to answer a question a process exit code cannot: "did this
// daemon die, or did somebody stop it?".
//
// It cannot be answered from the code alone. `unarr stop` on Windows is
// taskkill /f, so a user-initiated stop exits exactly like an AV kill; and the
// auto-upgrade exits 0 on purpose but DOES want a respawn. So intent is
// recorded out-of-band instead: whoever stops the daemon deliberately drops
// this marker first, and the shim reads it to decide between "stay down" and
// "bring it back".
//
// Absent marker means "nobody asked for this" — the safe default, because the
// failure it guards against (agent dead until the next logon) is worse than an
// unnecessary respawn.

// StopIntentFileName is the marker file, kept next to the daemon state file so
// both live in one data dir (and one uninstall sweep clears both). Exported
// because the Windows launcher shim has to embed this path at INSTALL time —
// it runs after the daemon process is gone, so it cannot ask the daemon where
// its data dir is.
const StopIntentFileName = "daemon.stopped"

// StopIntentPath is the marker location. Derived from StateFilePath so a test
// override (or a redirected data dir) moves both together.
func StopIntentPath() string {
	return filepath.Join(filepath.Dir(StateFilePath()), StopIntentFileName)
}

// WriteStopIntent records that the next daemon exit is deliberate and must NOT
// be respawned by the supervisor. Call it BEFORE stopping the process, so the
// marker is on disk by the time the launcher shim observes the exit.
//
// Best-effort: a marker that cannot be written degrades to an unnecessary
// restart, never to a stuck daemon.
func WriteStopIntent() {
	path := StopIntentPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte("stopped on purpose — do not respawn\n"), 0o644)
}

// ClearStopIntent consumes the marker. The daemon calls this as it starts: it is
// running again, so any previous stop request has been served and must not
// suppress the respawn after a LATER crash.
func ClearStopIntent() {
	_ = os.Remove(StopIntentPath())
}

// StopIntentExists reports whether a deliberate stop was recorded. Used by
// `unarr status`-style surfaces; the launcher shim does its own check in
// VBScript (it runs after the daemon process is already gone).
func StopIntentExists() bool {
	_, err := os.Stat(StopIntentPath())
	return err == nil
}
