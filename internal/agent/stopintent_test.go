package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// redirectStateDir points the state file (and therefore the stop-intent marker)
// at a temp dir for the duration of a test.
func redirectStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := stateFilePathFn
	stateFilePathFn = func() string { return filepath.Join(dir, "daemon.state.json") }
	t.Cleanup(func() { stateFilePathFn = orig })
	return dir
}

func TestStopIntentPathSitsBesideTheStateFile(t *testing.T) {
	dir := redirectStateDir(t)
	want := filepath.Join(dir, StopIntentFileName)
	if got := StopIntentPath(); got != want {
		t.Errorf("StopIntentPath() = %q, want %q", got, want)
	}
}

func TestStopIntentWriteClearRoundTrip(t *testing.T) {
	redirectStateDir(t)

	if StopIntentExists() {
		t.Fatal("no stop was requested yet, marker must be absent")
	}
	WriteStopIntent()
	if !StopIntentExists() {
		t.Fatal("WriteStopIntent did not record the intent")
	}
	// Writing twice must stay idempotent — `unarr stop` on an already-stopping
	// daemon should not turn into an error path.
	WriteStopIntent()
	if !StopIntentExists() {
		t.Fatal("second WriteStopIntent lost the marker")
	}
	ClearStopIntent()
	if StopIntentExists() {
		t.Fatal("ClearStopIntent did not consume the marker")
	}
	// Clearing an absent marker is the normal startup case (no stop was ever
	// requested) and must not blow up.
	ClearStopIntent()
}

// TestStopIntentCreatesMissingDir covers a first-ever stop on a box where the
// data dir does not exist yet: the marker must still land, because a marker that
// silently fails to write means the supervisor resurrects a daemon the user
// deliberately paused.
func TestStopIntentCreatesMissingDir(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "does", "not", "exist")
	orig := stateFilePathFn
	stateFilePathFn = func() string { return filepath.Join(nested, "daemon.state.json") }
	t.Cleanup(func() { stateFilePathFn = orig })

	WriteStopIntent()
	if !StopIntentExists() {
		t.Fatal("marker was not created under a missing data dir")
	}
}

// TestStopIntentMarkerIsReadable guards the shim's side of the contract: the
// VBScript launcher only does FileExists, so the file must be a plain readable
// regular file, not a directory or a zero-permission stub.
func TestStopIntentMarkerIsReadable(t *testing.T) {
	redirectStateDir(t)
	WriteStopIntent()

	info, err := os.Stat(StopIntentPath())
	if err != nil {
		t.Fatalf("stat marker: %v", err)
	}
	if info.IsDir() {
		t.Fatal("marker must be a file, not a directory")
	}
	if _, err := os.ReadFile(StopIntentPath()); err != nil {
		t.Errorf("marker is not readable: %v", err)
	}
}
