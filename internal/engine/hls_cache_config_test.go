package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// seedSealedEntry writes a complete-looking cache entry so the reconcile tests
// have something with real contents to keep or drop.
func seedSealedEntry(t *testing.T, c *HLSCache, key string) string {
	t.Helper()
	dir := c.DirFor(key)
	if err := os.MkdirAll(filepath.Join(dir, "video"), 0o755); err != nil {
		t.Fatalf("mkdir entry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "video", "init.mp4"), []byte("init"), 0o644); err != nil {
		t.Fatalf("write init.mp4: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "video", "seg-0.m4s"), []byte("seg"), 0o644); err != nil {
		t.Fatalf("write seg-0: %v", err)
	}
	if err := c.MarkComplete(key); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	return dir
}

// TestReconcileKeepsCacheOnFirstRun covers the upgrade path. A cache built
// before stamping exists carries no stamp, but its entries were produced by
// the same binary under the same settings — discarding them would re-encode
// the user's whole library for nothing.
func TestReconcileKeepsCacheOnFirstRun(t *testing.T) {
	c := newTestCache(t, 1)
	dir := seedSealedEntry(t, c, "entry1")

	removed, err := c.ReconcileEncodeConfig("none|veryfast|6000k|128k|1080|false")
	if err != nil {
		t.Fatalf("ReconcileEncodeConfig: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed %d entries from an unstamped cache; an upgrade must not "+
			"invalidate encodes it has no evidence against", removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("entry was deleted on first run: %v", err)
	}
}

// TestReconcileKeepsCacheWhenSettingsUnchanged is the ordinary restart: the
// cache must survive, or every daemon restart would cost a full re-encode.
func TestReconcileKeepsCacheWhenSettingsUnchanged(t *testing.T) {
	c := newTestCache(t, 1)
	const fp = "none|veryfast|6000k|128k|1080|false"
	if _, err := c.ReconcileEncodeConfig(fp); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	dir := seedSealedEntry(t, c, "entry1")

	removed, err := c.ReconcileEncodeConfig(fp)
	if err != nil {
		t.Fatalf("ReconcileEncodeConfig: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed %d entries without a settings change", removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("entry was deleted on an unchanged restart: %v", err)
	}
}

// TestReconcileDropsCacheWhenSettingsChange is the point of the mechanism.
// Entries encoded under the old settings can never be HIT again — the settings
// feed the cache key — so they would hold the budget until the LRU happened to
// reach them, and they are exactly the entries a user is trying to get rid of
// when they change the config.
func TestReconcileDropsCacheWhenSettingsChange(t *testing.T) {
	c := newTestCache(t, 1)
	if _, err := c.ReconcileEncodeConfig("none|veryfast|6000k|128k|1080|false"); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	dirs := []string{
		seedSealedEntry(t, c, "entry1"),
		seedSealedEntry(t, c, "entry2"),
	}

	// hwaccel auto -> videotoolbox, preset and bitrate changed with it.
	removed, err := c.ReconcileEncodeConfig("videotoolbox|medium|10M|128k|1080|false")
	if err != nil {
		t.Fatalf("ReconcileEncodeConfig: %v", err)
	}
	if removed != len(dirs) {
		t.Fatalf("removed %d entries, want %d", removed, len(dirs))
	}
	for _, d := range dirs {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("%s survived a settings change; it can never be HIT again "+
				"yet still holds the cache budget", d)
		}
	}
	// The stamp itself must survive, holding the new settings — otherwise the
	// next restart reads an unstamped cache and the drop happens again.
	if _, err := os.Stat(filepath.Join(c.root, hlsCacheStampFile)); err != nil {
		t.Fatalf("stamp missing after reconcile: %v", err)
	}
}

// TestReconcileIsStableAcrossRestarts pins that a settings change costs one
// drop, not one per restart.
func TestReconcileIsStableAcrossRestarts(t *testing.T) {
	c := newTestCache(t, 1)
	if _, err := c.ReconcileEncodeConfig("none|veryfast|6000k|128k|1080|false"); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	const newFP = "videotoolbox|medium|10M|128k|1080|false"
	if _, err := c.ReconcileEncodeConfig(newFP); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	dir := seedSealedEntry(t, c, "encoded-under-new-settings")
	removed, err := c.ReconcileEncodeConfig(newFP)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed %d entries on a restart with unchanged settings", removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("entry encoded under the current settings was dropped: %v", err)
	}
}

// TestReconcileTreatsCorruptStampAsUnstamped: the stamp is a validity hint, so
// a truncated or malformed file must not fail the daemon's startup. Treating
// it as absent costs at most one avoidable re-encode later.
func TestReconcileTreatsCorruptStampAsUnstamped(t *testing.T) {
	c := newTestCache(t, 1)
	if err := os.WriteFile(filepath.Join(c.root, hlsCacheStampFile), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt stamp: %v", err)
	}
	dir := seedSealedEntry(t, c, "entry1")

	removed, err := c.ReconcileEncodeConfig("none|veryfast|6000k|128k|1080|false")
	if err != nil {
		t.Fatalf("a corrupt stamp must not fail startup: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed %d entries on a corrupt stamp; unreadable is not evidence "+
			"the entries are wrong", removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("entry dropped on a corrupt stamp: %v", err)
	}
	// And the stamp must be repaired, so the next restart has a valid one.
	if got, err := readHLSCacheStamp(filepath.Join(c.root, hlsCacheStampFile)); err != nil || got == "" {
		t.Fatalf("stamp not rewritten: got %q err %v", got, err)
	}
}

// TestReconcileLeavesNonDirEntriesAlone guards the sweep: the stamp lives at
// the cache root alongside the per-key directories, so a sweep that deleted
// files as well as directories would delete its own record.
func TestReconcileLeavesNonDirEntriesAlone(t *testing.T) {
	c := newTestCache(t, 1)
	if _, err := c.ReconcileEncodeConfig("a|b|c|d|1|false"); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	seedSealedEntry(t, c, "entry1")

	if _, err := c.ReconcileEncodeConfig("z|y|x|w|2|true"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := readHLSCacheStamp(filepath.Join(c.root, hlsCacheStampFile))
	if err != nil {
		t.Fatalf("read stamp: %v", err)
	}
	if got != "z|y|x|w|2|true" {
		t.Fatalf("stamp = %q, want the new fingerprint", got)
	}
}
