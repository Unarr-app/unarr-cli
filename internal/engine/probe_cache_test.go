package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProbeCache_LookupMissNonexistent(t *testing.T) {
	ResetProbeCache()
	t.Cleanup(ResetProbeCache)

	if _, ok := lookupProbeCache("/path/that/does/not/exist"); ok {
		t.Fatal("expected MISS for non-existent path")
	}
}

func TestProbeCache_StoreThenLookupHit(t *testing.T) {
	ResetProbeCache()
	t.Cleanup(ResetProbeCache)

	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(path, []byte("fake content"), 0o644); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}

	probe := &StreamProbe{VideoCodec: "h264", Width: 1920, Height: 1080, DurationSec: 5400}
	storeProbeCache(path, probe)

	got, ok := lookupProbeCache(path)
	if !ok {
		t.Fatal("expected HIT after store")
	}
	if got != probe {
		t.Fatalf("expected pointer-identical probe; got different")
	}
}

func TestProbeCache_MtimeChangeInvalidates(t *testing.T) {
	ResetProbeCache()
	t.Cleanup(ResetProbeCache)

	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	probe := &StreamProbe{VideoCodec: "h264", DurationSec: 100}
	storeProbeCache(path, probe)

	// Force mtime change. WriteFile doesn't guarantee a different mtime if
	// the filesystem timestamp resolution is coarse, so set it explicitly
	// to a value 1 hour in the future.
	future := time.Now().Add(1 * time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if _, ok := lookupProbeCache(path); ok {
		t.Fatal("expected MISS after mtime change")
	}
}

func TestProbeCache_SizeChangeInvalidates(t *testing.T) {
	ResetProbeCache()
	t.Cleanup(ResetProbeCache)

	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(path, []byte("aaaaa"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	probe := &StreamProbe{VideoCodec: "h264", DurationSec: 100}
	storeProbeCache(path, probe)

	// Truncate file to a different size + reset mtime to the original (to
	// isolate the size-check path). Stat picks up new size immediately.
	if err := os.WriteFile(path, []byte("a"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	if _, ok := lookupProbeCache(path); ok {
		t.Fatal("expected MISS after size change")
	}
}

func TestProbeCache_ExpiryDropsEntry(t *testing.T) {
	ResetProbeCache()
	t.Cleanup(ResetProbeCache)

	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Stash an entry whose expires is already in the past — simulates TTL
	// having elapsed without sleeping for 30 min.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	key := probeCacheKey{path: path, mtime: fi.ModTime().UnixNano(), size: fi.Size()}
	probeCacheMu.Lock()
	probeCache[key] = probeCacheEntry{
		probe:   &StreamProbe{VideoCodec: "h264"},
		expires: time.Now().Add(-1 * time.Minute),
	}
	probeCacheMu.Unlock()

	if _, ok := lookupProbeCache(path); ok {
		t.Fatal("expected MISS for expired entry")
	}
	// Side-effect: lookup should have evicted the stale entry.
	if ProbeCacheSize() != 0 {
		t.Fatalf("expected cache size 0 after expiry eviction; got %d", ProbeCacheSize())
	}
}

func TestProbeCache_ResetClears(t *testing.T) {
	ResetProbeCache()

	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	storeProbeCache(path, &StreamProbe{VideoCodec: "h264"})
	if ProbeCacheSize() != 1 {
		t.Fatalf("expected size 1 after store; got %d", ProbeCacheSize())
	}

	ResetProbeCache()
	if ProbeCacheSize() != 0 {
		t.Fatalf("expected size 0 after reset; got %d", ProbeCacheSize())
	}
}

func TestProbeCache_StoreNonexistentNoOp(t *testing.T) {
	ResetProbeCache()
	t.Cleanup(ResetProbeCache)

	// Store on a non-existent path should silently do nothing (stat fails),
	// not panic, and not poison the cache with a zero key.
	storeProbeCache("/nope/never/exists.mkv", &StreamProbe{VideoCodec: "h264"})
	if ProbeCacheSize() != 0 {
		t.Fatalf("expected 0 entries; got %d", ProbeCacheSize())
	}
}
