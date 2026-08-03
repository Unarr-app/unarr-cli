package davfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library"
)

// bumpMtime pushes a file's mtime forward so reloadLocked's "unchanged since last
// load" guard lets the next refresh through to a rebuild.
func bumpMtime(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	nt := fi.ModTime().Add(time.Hour)
	if err := os.Chtimes(path, nt, nt); err != nil {
		t.Fatal(err)
	}
}

// forceRefresh clears the refresh throttle so the next FS call re-stats the cache.
// A 1ns RefreshInterval is not enough on its own: Windows' monotonic clock ticks
// at ~15ms, so time.Since(lastStat) reads 0 and the throttle swallows the reload
// the test is about. Zeroing the timestamp makes the trigger explicit everywhere.
func forceRefresh(f *FS) {
	f.mu.Lock()
	f.lastStat = time.Time{}
	f.mu.Unlock()
}

// TestReloadKeepsGoodTreeOnLoadError: the daemon serves WebDAV continuously while
// auto-scan rewrites library.json, so a corrupt/half-written cache (Load returns
// an error) must NOT break the mount — the previous good tree is kept and requests
// keep succeeding.
func TestReloadKeepsGoodTreeOnLoadError(t *testing.T) {
	root := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "library.json")
	// A real file so reloadLocked can stat it for the mtime guard; its bytes are
	// irrelevant because Load is the seam.
	if err := os.WriteFile(cachePath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	good := &library.LibraryCache{Items: []library.LibraryItem{
		mediaItem(t, root, "dl/Inception.2010.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "Inception", "2010" }),
	}}
	loadCache, loadErr := good, error(nil)
	f := New(Options{
		Load:            func() (*library.LibraryCache, error) { return loadCache, loadErr },
		CachePath:       cachePath,
		RefreshInterval: time.Nanosecond, // always attempt a refresh; the mtime guard gates the rebuild
	})

	// First refresh builds the tree from the good cache.
	if _, err := f.Stat(context.Background(), "/Movies/Inception (2010)/Inception.2010.mkv"); err != nil {
		t.Fatalf("first load: leaf missing: %v", err)
	}

	// Cache "changes" (mtime bumped) but Load now fails — the previous good tree
	// must be kept and the mount stays usable.
	bumpMtime(t, cachePath)
	forceRefresh(f)
	loadCache, loadErr = nil, errors.New("corrupt/half-written cache")

	if _, err := f.Stat(context.Background(), "/Movies/Inception (2010)/Inception.2010.mkv"); err != nil {
		t.Errorf("after a Load error the previous good tree must be kept; leaf err = %v", err)
	}
	if fi, err := f.Stat(context.Background(), "/"); err != nil || !fi.IsDir() {
		t.Errorf("root must stay a browsable dir after a Load error: fi=%v err=%v", fi, err)
	}
}

// TestReloadNilCacheEmptyTree: Load returning (nil, nil) means "no library yet" —
// the root becomes an empty browsable directory (not a crash, not the stale tree).
func TestReloadNilCacheEmptyTree(t *testing.T) {
	root := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "library.json")
	if err := os.WriteFile(cachePath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	good := &library.LibraryCache{Items: []library.LibraryItem{
		mediaItem(t, root, "dl/M.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "M", "2020" }),
	}}
	loadCache, loadErr := good, error(nil)
	f := New(Options{
		Load:            func() (*library.LibraryCache, error) { return loadCache, loadErr },
		CachePath:       cachePath,
		RefreshInterval: time.Nanosecond,
	})

	statDir(t, f, "/Movies/M (2020)") // good cache first

	bumpMtime(t, cachePath)
	forceRefresh(f)
	loadCache = nil // (nil, nil) → empty tree

	if _, err := f.Stat(context.Background(), "/Movies/M (2020)"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale movie should be gone after a nil cache; err = %v, want ErrNotExist", err)
	}
	if fi, err := f.Stat(context.Background(), "/"); err != nil || !fi.IsDir() {
		t.Errorf("root must be a browsable empty dir after a nil cache: fi=%v err=%v", fi, err)
	}
	if names := listNames(t, f, "/"); len(names) != 0 {
		t.Errorf("root listing after a nil cache = %v, want []", names)
	}
}

// TestReloadSkipsRebuildWhenMtimeUnchanged: an unchanged cache mtime on the next
// refresh makes reloadLocked return early — no re-load, and the tree object is NOT
// swapped (avoids rebuilding on every request).
func TestReloadSkipsRebuildWhenMtimeUnchanged(t *testing.T) {
	root := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "library.json")
	if err := os.WriteFile(cachePath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	good := &library.LibraryCache{Items: []library.LibraryItem{
		mediaItem(t, root, "dl/M.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "M", "2020" }),
	}}
	loadCalls := 0
	f := New(Options{
		Load:            func() (*library.LibraryCache, error) { loadCalls++; return good, nil },
		CachePath:       cachePath,
		RefreshInterval: time.Nanosecond,
	})

	statDir(t, f, "/Movies/M (2020)") // first access → one load
	if loadCalls != 1 {
		t.Fatalf("loadCalls after first access = %d, want 1", loadCalls)
	}
	f.mu.RLock()
	before := f.root
	f.mu.RUnlock()

	// Second access WITHOUT bumping the mtime → early return: no new load, same tree.
	forceRefresh(f)
	statDir(t, f, "/Movies/M (2020)")
	if loadCalls != 1 {
		t.Errorf("loadCalls after an unchanged-mtime refresh = %d, want still 1 (no rebuild)", loadCalls)
	}
	f.mu.RLock()
	after := f.root
	f.mu.RUnlock()
	if before != after {
		t.Error("root tree was swapped on an unchanged cache; want the same object (early return)")
	}
}
