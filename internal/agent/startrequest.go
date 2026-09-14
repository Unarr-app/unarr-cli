package agent

import (
	"os"
	"path/filepath"
)

// Start-now request marker — the counterpart of the stop-intent marker.
//
// After a crash the Windows launcher shim waits out a backoff (15 s doubling to
// two minutes) before relaunching, and the scheduled task stays Running the
// whole time. `schtasks /run` on a Running task with MultipleInstancesPolicy
// IgnoreNew succeeds and starts nothing, so a user who clicked Resume in the
// tray right after a crash saw nothing happen for up to two minutes. The shim
// polls for this marker during its backoff instead of sleeping blind: present →
// relaunch now, with a fresh failure budget, because a person asked for it.
//
// Absent marker means "keep waiting" — the backoff exists so a broken install
// is not a spin loop, and only an explicit start request may cut it short.

// StartRequestFileName is the marker file, next to the state file like
// StopIntentFileName, and exported for the same reason: the shim embeds the
// path at install time.
const StartRequestFileName = "daemon.start-now"

// StartRequestPath is the marker location, derived from StateFilePath so a test
// override moves it with everything else.
func StartRequestPath() string {
	return filepath.Join(filepath.Dir(StateFilePath()), StartRequestFileName)
}

// WriteStartRequest asks a shim sitting in its relaunch backoff to relaunch now.
// Best-effort: a marker that cannot be written costs the rest of the backoff,
// which is what happened before it existed.
func WriteStartRequest() {
	path := StartRequestPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte("start requested — skip the relaunch backoff\n"), 0o644)
}

// ClearStartRequest consumes the marker. The shim deletes it when it acts on
// it; the daemon also clears it once it holds the instance lock, so a request
// that found no shim in backoff (the task was not running, or a daemon was
// already up) cannot linger and cut short the backoff after some LATER crash.
func ClearStartRequest() {
	_ = os.Remove(StartRequestPath())
}
