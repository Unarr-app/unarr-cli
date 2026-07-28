package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
)

// fakePending is a test double for pendingDownloadCounter: it returns a fixed set
// of non-terminal tasks (simulating paused/interrupted downloads in the resume
// store).
type fakePending struct{ tasks []agent.Task }

func (f fakePending) Load() []agent.Task { return f.tasks }

// cleanupCfg builds a minimal config with the hygiene sweep enabled against dir.
func cleanupCfg(dir string) config.Config {
	var cfg config.Config
	cfg.Download.Dir = dir
	cfg.Library.Cleanup = config.CleanupConfig{
		Enabled:               true,
		RemoveStubs:           true,
		RemoveOrphanPartials:  true,
		DedupExact:            true,
		RemoveOrphanSubtitles: true,
		PruneEmptyDirs:        true,
	}
	return cfg
}

// writeOldPartial creates a .part with a mtime well past the 5-min active window,
// so ONLY the pending-task protection (not the mtime heuristic) can save it.
func writeOldPartial(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// TestRunPostScanCleanupProtectsPausedPartial is the #6 regression: an OLD-mtime
// .part must NOT be reaped by the auto-sweep while the resume store reports a
// pending (paused/interrupted) download — otherwise resume is impossible.
func TestRunPostScanCleanupProtectsPausedPartial(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "paused-download.part")
	writeOldPartial(t, part)

	// A pending (paused) task exists → orphan-partial removal must be skipped.
	pending := fakePending{tasks: []agent.Task{{ID: "task-1", Title: "Paused Movie"}}}
	runPostScanCleanup(cleanupCfg(dir), pending)

	if _, err := os.Stat(part); err != nil {
		t.Errorf("an old .part must survive while a pending download exists (resume protection): %v", err)
	}
}

// TestRunPostScanCleanupReapsWhenNoPending confirms the normal path is intact: with
// NO pending downloads, an old orphan .part IS reaped.
func TestRunPostScanCleanupReapsWhenNoPending(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "orphan.part")
	writeOldPartial(t, part)

	pending := fakePending{tasks: nil} // no non-terminal downloads
	runPostScanCleanup(cleanupCfg(dir), pending)

	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Errorf("with no pending downloads, an old orphan .part should be reaped (err=%v)", err)
	}
}

// TestRunPostScanCleanupNilPendingReaps: a nil counter (no resume store wired) must
// not panic and falls back to the mtime heuristic — an old orphan is reaped.
func TestRunPostScanCleanupNilPendingReaps(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "orphan.part")
	writeOldPartial(t, part)

	runPostScanCleanup(cleanupCfg(dir), nil)

	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Errorf("nil pending → mtime heuristic reaps an old orphan (err=%v)", err)
	}
}
