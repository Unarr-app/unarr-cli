package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestCache(t *testing.T, sizeGB int) *HLSCache {
	t.Helper()
	root := t.TempDir()
	c, err := NewHLSCache(root, sizeGB)
	if err != nil {
		t.Fatalf("NewHLSCache: %v", err)
	}
	return c
}

func TestKeyForStable(t *testing.T) {
	c := newTestCache(t, 1)
	k1 := c.KeyFor("/a/b/movie.mkv", "1080p", 0)
	k2 := c.KeyFor("/a/b/movie.mkv", "1080p", 0)
	if k1 != k2 {
		t.Fatalf("expected stable keys, got %q vs %q", k1, k2)
	}
	if c.KeyFor("/a/b/movie.mkv", "720p", 0) == k1 {
		t.Fatal("quality should change key")
	}
	if c.KeyFor("/a/b/movie.mkv", "1080p", 1) == k1 {
		t.Fatal("audio index should change key")
	}
	if c.KeyFor("/x/y/other.mkv", "1080p", 0) == k1 {
		t.Fatal("path should change key")
	}
}

func TestMarkCompleteAndHas(t *testing.T) {
	c := newTestCache(t, 1)
	key := "abc123"
	if c.HasComplete(key) {
		t.Fatal("fresh cache should not report complete")
	}
	// Production callers create the dir during StartHLSSession; MarkComplete
	// trusts that invariant and fails if the dir was wiped meanwhile.
	if err := os.MkdirAll(c.DirFor(key), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := c.MarkComplete(key); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	if !c.HasComplete(key) {
		t.Fatal("after MarkComplete, HasComplete must be true")
	}
}

func TestMarkCompleteFailsWithoutDir(t *testing.T) {
	c := newTestCache(t, 1)
	if err := c.MarkComplete("never-created"); err == nil {
		t.Fatal("MarkComplete should error when dir doesn't exist")
	}
}

func TestPinPreventsEviction(t *testing.T) {
	c := newTestCache(t, 1) // 1 GB budget, but min clamp keeps it usable
	c.maxBytes = 1024       // squeeze budget for the test

	// Write two entries past the budget.
	for i, key := range []string{"old", "new"} {
		dir := c.DirFor(key)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		path := filepath.Join(dir, "seg.bin")
		if err := os.WriteFile(path, make([]byte, 800), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		now := time.Now().Add(time.Duration(i) * time.Hour) // "old" mtime < "new"
		_ = os.Chtimes(dir, now, now)
	}

	c.Pin("old") // protect the older one
	freed, err := c.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if freed == 0 {
		t.Fatal("expected some eviction")
	}
	if _, err := os.Stat(c.DirFor("old")); err != nil {
		t.Fatal("pinned 'old' was evicted")
	}
	if _, err := os.Stat(c.DirFor("new")); err == nil {
		t.Fatal("'new' should have been evicted to make room")
	}
}

func TestSweepNoOpUnderBudget(t *testing.T) {
	c := newTestCache(t, 1)
	dir := c.DirFor("small")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "x"), []byte("tiny"), 0o644)
	freed, err := c.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if freed != 0 {
		t.Fatalf("expected 0 freed under budget, got %d", freed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("under-budget entry was wrongly evicted")
	}
}

func TestSweepEmptyRoot(t *testing.T) {
	c := newTestCache(t, 1)
	freed, err := c.Sweep()
	if err != nil {
		t.Fatalf("Sweep empty: %v", err)
	}
	if freed != 0 {
		t.Fatalf("freed=%d, want 0", freed)
	}
}

func TestInvalidateRemovesDir(t *testing.T) {
	c := newTestCache(t, 1)
	key := "drop"
	dir := c.DirFor(key)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "x"), []byte("y"), 0o644)
	if err := c.Invalidate(key); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("dir still present after Invalidate")
	}
}

func TestTouchUpdatesMtime(t *testing.T) {
	c := newTestCache(t, 1)
	key := "touch"
	dir := c.DirFor(key)
	_ = os.MkdirAll(dir, 0o755)
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(dir, old, old)

	if err := c.Touch(key); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().After(old.Add(time.Minute)) {
		t.Fatalf("mtime not refreshed: %v", info.ModTime())
	}
}

func TestPinUnpinSymmetry(t *testing.T) {
	c := newTestCache(t, 1)
	c.Pin("k")
	c.Pin("k")
	if !c.isPinned("k") {
		t.Fatal("Pin twice should leave pinned")
	}
	c.Unpin("k")
	if !c.isPinned("k") {
		t.Fatal("Unpin once should keep pinned (refs=1)")
	}
	c.Unpin("k")
	if c.isPinned("k") {
		t.Fatal("Unpin twice should drop pin")
	}
	c.Unpin("k") // safe no-op
}

func TestConcurrentPinUnpin(t *testing.T) {
	c := newTestCache(t, 1)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Pin("race")
			time.Sleep(time.Microsecond)
			c.Unpin("race")
		}()
	}
	wg.Wait()
	if c.isPinned("race") {
		t.Fatal("refs leaked")
	}
}

func TestSweeperLoopExits(t *testing.T) {
	c := newTestCache(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	c.StartSweeper(ctx, 10*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	cancel()
	// If StartSweeper doesn't exit on cancel the test would leak a goroutine;
	// the leak detector in the test runner will surface it.
	time.Sleep(20 * time.Millisecond)
}

func TestMinBudgetClamp(t *testing.T) {
	root := t.TempDir()
	c, err := NewHLSCache(root, 0) // below floor
	if err != nil {
		t.Fatalf("NewHLSCache: %v", err)
	}
	if c.maxBytes != int64(hlsCacheMinBudgetGB)*1024*1024*1024 {
		t.Fatalf("budget not clamped to min: got %d", c.maxBytes)
	}
}

func TestTryAcquireWriterExclusive(t *testing.T) {
	c := newTestCache(t, 1)
	if !c.TryAcquireWriter("k") {
		t.Fatal("first acquire should succeed")
	}
	if c.TryAcquireWriter("k") {
		t.Fatal("second acquire for same key must fail")
	}
	if !c.TryAcquireWriter("other") {
		t.Fatal("different key should not conflict")
	}
	c.ReleaseWriter("k")
	if !c.TryAcquireWriter("k") {
		t.Fatal("acquire after release should succeed")
	}
	c.ReleaseWriter("k")
	c.ReleaseWriter("k") // idempotent
}

func TestStartupOrphanCleanup(t *testing.T) {
	root := t.TempDir()

	// Pre-seed: one sealed dir + one orphan old enough + one orphan fresh.
	sealed := filepath.Join(root, "sealed")
	_ = os.MkdirAll(sealed, 0o755)
	_ = os.WriteFile(filepath.Join(sealed, hlsCacheCompleteMarker), nil, 0o644)

	staleOrphan := filepath.Join(root, "stale_orphan")
	_ = os.MkdirAll(staleOrphan, 0o755)
	old := time.Now().Add(-2 * hlsCacheStartupOrphanAge)
	_ = os.Chtimes(staleOrphan, old, old)

	freshOrphan := filepath.Join(root, "fresh_orphan")
	_ = os.MkdirAll(freshOrphan, 0o755)

	if _, err := NewHLSCache(root, 1); err != nil {
		t.Fatalf("NewHLSCache: %v", err)
	}

	if _, err := os.Stat(sealed); err != nil {
		t.Fatal("sealed dir was wrongly removed")
	}
	if _, err := os.Stat(staleOrphan); err == nil {
		t.Fatal("stale orphan should have been removed at startup")
	}
	if _, err := os.Stat(freshOrphan); err != nil {
		t.Fatal("fresh orphan should be kept (might be a mid-restart encode)")
	}
}

func TestHitMissCounters(t *testing.T) {
	c := newTestCache(t, 1)
	if s := c.Stats(); s.Hits != 0 || s.Misses != 0 {
		t.Fatalf("fresh cache stats not zero: %+v", s)
	}
	c.RecordHit()
	c.RecordHit()
	c.RecordMiss()
	s := c.Stats()
	if s.Hits != 2 || s.Misses != 1 {
		t.Fatalf("counters wrong: %+v", s)
	}
	// 2/3 = 67%
	if got := c.hitRatePercent(); got != 67 {
		t.Fatalf("hitRatePercent=%d, want 67", got)
	}
}

func TestStatsEntryCount(t *testing.T) {
	c := newTestCache(t, 1)
	for _, k := range []string{"a", "b", "c"} {
		dir := c.DirFor(k)
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "x"), []byte("hello"), 0o644)
	}
	s := c.Stats()
	if s.EntryCount != 3 {
		t.Fatalf("EntryCount=%d, want 3", s.EntryCount)
	}
	if s.TotalBytes != 15 {
		t.Fatalf("TotalBytes=%d, want 15", s.TotalBytes)
	}
}

func TestVerifyCompleteRejectsMissingFiles(t *testing.T) {
	c := newTestCache(t, 1)
	key := "v"
	dir := c.DirFor(key)
	_ = os.MkdirAll(filepath.Join(dir, "video"), 0o755)

	// No .complete yet → reject.
	if c.VerifyComplete(key, 2) {
		t.Fatal("VerifyComplete should reject without .complete")
	}

	// Mark complete but no files → reject.
	if err := c.MarkComplete(key); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	if c.VerifyComplete(key, 2) {
		t.Fatal("VerifyComplete should reject when init.mp4 missing")
	}

	// Write init.mp4, last seg missing → reject.
	_ = os.WriteFile(filepath.Join(dir, "video", "init.mp4"), []byte("..."), 0o644)
	if c.VerifyComplete(key, 2) {
		t.Fatal("VerifyComplete should reject when last segment missing")
	}

	// Write last seg → pass.
	_ = os.WriteFile(filepath.Join(dir, "video", "seg-1.m4s"), []byte("..."), 0o644)
	if !c.VerifyComplete(key, 2) {
		t.Fatal("VerifyComplete should pass with all files present")
	}

	// Zero-size last seg → reject.
	_ = os.WriteFile(filepath.Join(dir, "video", "seg-1.m4s"), nil, 0o644)
	if c.VerifyComplete(key, 2) {
		t.Fatal("VerifyComplete should reject zero-size last segment")
	}
}

func TestSweepRespectsPinnedExceedsBudget(t *testing.T) {
	c := newTestCache(t, 1)
	c.maxBytes = 256 // squeeze

	pinned := c.DirFor("pinned")
	_ = os.MkdirAll(pinned, 0o755)
	_ = os.WriteFile(filepath.Join(pinned, "x"), make([]byte, 1024), 0o644)
	c.Pin("pinned")

	freed, err := c.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if freed != 0 {
		t.Fatalf("nothing should have been freed: got %d", freed)
	}
	if _, err := os.Stat(pinned); err != nil {
		t.Fatal("pinned dir wrongly removed despite over-budget pin")
	}
}
