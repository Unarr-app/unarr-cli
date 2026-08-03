package agent

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// StatusStarting is the state a daemon publishes before it has registered: the
// process is up and already writing its log, but it has no agent identity yet.
// Deliberately NOT "running" — the tray reads a "running" state whose PID is
// gone as a crash worth reporting, and a start that dies before registering was
// never a running agent.
const StatusStarting = "starting"

// ownedLogFile is the log file THIS process owns and rotates from the inside
// (`unarr start --log-file …`). Empty means this process owns none.
//
// It exists because ownership CANNOT be probed. A rotator outside the daemon
// used to ask the filesystem "may I truncate this?", and the answer is yes even
// while the daemon is writing: Go opens files with FILE_SHARE_WRITE on Windows
// and takes no lock at all on POSIX. The only reliable answer comes from the
// owner having said so, which is what this value plus the state file's PID
// (checked for liveness by the reader) is.
var ownedLogFile atomic.Value // string

// OwnedLogFile returns the log file this process owns, or "".
func OwnedLogFile() string {
	s, _ := ownedLogFile.Load().(string)
	return s
}

// ClaimLogFile records that this process owns path and publishes the claim to
// the state file so external rotators can see it.
//
// Publishing immediately, rather than waiting for the first heartbeat, is the
// point: registration needs the network, and a daemon parked offline can be
// writing to its log for hours before it ever writes a registered state. Every
// later WriteState re-stamps the claim, so a full-state overwrite cannot drop
// it.
//
// version is this build's version, used only when the claim has to create the
// state file from nothing.
func ClaimLogFile(path, version string) {
	if path == "" {
		return
	}
	ownedLogFile.Store(filepath.Clean(path))
	publishLogClaim(version)
}

// ReleaseLogFile drops the claim. Called when the Writer is closed, which on
// every real path is the daemon shutting down — by then the state file is being
// reaped anyway, so this only has to stop any straggler write from re-stamping
// a file this process no longer owns.
func ReleaseLogFile() { ownedLogFile.Store("") }

// publishLogClaim writes the claim into the state file, without ever
// overwriting the record of a DIFFERENT live daemon: a dev agent and the
// production agent deliberately share one data dir (only the lock is
// config-scoped), and stealing that file would make `unarr stop` aim at the
// wrong process.
func publishLogClaim(version string) {
	st, err := LoadState()
	if err == nil && st.PID != os.Getpid() && IsProcessAlive(st.PID) {
		return
	}
	if err == nil && st.PID == os.Getpid() {
		WriteState(st) // keep the live record, add the claim
		return
	}
	WriteState(&DaemonState{
		Status:    StatusStarting,
		Version:   version,
		PID:       os.Getpid(),
		StartedAt: time.Now(),
	})
}
