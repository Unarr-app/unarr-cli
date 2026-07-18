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

// saveCache overwrites the library cache at cachePath with items.
func saveCache(t *testing.T, cachePath string, items []library.LibraryItem) {
	t.Helper()
	if err := library.SaveCacheTo(&library.LibraryCache{Items: items}, cachePath); err != nil {
		t.Fatalf("save cache: %v", err)
	}
}

// setMtime forces the cache file's mtime so the refresh throttle's change-check
// is deterministic regardless of the filesystem's timestamp resolution.
func setMtime(t *testing.T, path string, mod time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func liveFS(t *testing.T, cachePath string, every time.Duration) *FS {
	t.Helper()
	return New(Options{
		Load:            func() (*library.LibraryCache, error) { return library.LoadCacheFrom(cachePath) },
		CachePath:       cachePath,
		RefreshInterval: every,
	})
}

// TestLiveRefreshPicksUpNewDownload is the headline contract: a download that
// lands AFTER the FS is built appears on the next stat once the cache file's
// mtime advances past the refresh window — no daemon restart.
func TestLiveRefreshPicksUpNewDownload(t *testing.T) {
	root := t.TempDir()
	cachePath := filepath.Join(root, "library.json")
	itemA := mediaItem(t, root, "dl/A.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "Alpha", "2020" })
	saveCache(t, cachePath, []library.LibraryItem{itemA})
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	setMtime(t, cachePath, base)

	f := liveFS(t, cachePath, time.Millisecond)
	ctx := context.Background()

	// A is visible from the first stat; B does not exist yet.
	if _, err := f.Stat(ctx, "/Movies/Alpha (2020)"); err != nil {
		t.Fatalf("A folder missing at start: %v", err)
	}
	if _, err := f.Stat(ctx, "/Movies/Beta (2021)"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("B present before its download landed: err=%v, want ErrNotExist", err)
	}

	// A new download lands: rewrite the cache adding B, with a strictly newer mtime.
	itemB := mediaItem(t, root, "dl/B.2021.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "Beta", "2021" })
	saveCache(t, cachePath, []library.LibraryItem{itemA, itemB})
	setMtime(t, cachePath, base.Add(2*time.Second))

	// Past the (1ms) refresh window a fresh stat rebuilds the tree and B appears.
	time.Sleep(5 * time.Millisecond)
	if _, err := f.Stat(ctx, "/Movies/Beta (2021)"); err != nil {
		t.Errorf("B not visible after refresh: %v (new download should appear without a restart)", err)
	}
	if _, err := f.Stat(ctx, "/Movies/Alpha (2020)"); err != nil {
		t.Errorf("A disappeared after refresh: %v", err)
	}
}

// TestLiveRefreshThrottleHoldsWithinWindow: with a long refresh interval a change
// made inside the window is NOT yet visible — the throttle holds the old tree.
func TestLiveRefreshThrottleHoldsWithinWindow(t *testing.T) {
	root := t.TempDir()
	cachePath := filepath.Join(root, "library.json")
	itemA := mediaItem(t, root, "dl/A.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "Alpha", "2020" })
	saveCache(t, cachePath, []library.LibraryItem{itemA})
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	setMtime(t, cachePath, base)

	f := liveFS(t, cachePath, time.Hour) // long window
	ctx := context.Background()

	// First access loads A and arms the throttle (loadedOnce + lastStat = now).
	if _, err := f.Stat(ctx, "/Movies/Alpha (2020)"); err != nil {
		t.Fatalf("A missing at start: %v", err)
	}

	// B lands with a newer mtime, but still inside the 1h window.
	itemB := mediaItem(t, root, "dl/B.2021.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "Beta", "2021" })
	saveCache(t, cachePath, []library.LibraryItem{itemA, itemB})
	setMtime(t, cachePath, base.Add(2*time.Second))

	// The throttle short-circuits the reload: B is NOT visible yet.
	if _, err := f.Stat(ctx, "/Movies/Beta (2021)"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("B visible inside throttle window: err=%v, want ErrNotExist (throttle must hold the old tree)", err)
	}
	// A stays visible (the old tree is intact, not dropped).
	if _, err := f.Stat(ctx, "/Movies/Alpha (2020)"); err != nil {
		t.Errorf("A missing inside throttle window: %v", err)
	}
}

// TestResolveDescendPastFile: a path that continues PAST a leaf file resolves to
// nothing (not the file, not a panic) — both Stat and OpenFile report absent.
func TestResolveDescendPastFile(t *testing.T) {
	root := t.TempDir()
	f := newTestFS(t, root, []library.LibraryItem{
		mediaItem(t, root, "dl/M.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "M", "2020" }),
	}, nil)
	ctx := context.Background()

	deep := "/Movies/M (2020)/M.2020.mkv/anything"
	if _, err := f.Stat(ctx, deep); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat past leaf err = %v, want ErrNotExist", err)
	}
	if _, err := f.OpenFile(ctx, deep, os.O_RDONLY, 0); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OpenFile past leaf err = %v, want ErrNotExist", err)
	}
	// Sanity: the leaf ITSELF still resolves (the rejection isn't over-broad).
	if _, err := f.Stat(ctx, "/Movies/M (2020)/M.2020.mkv"); err != nil {
		t.Errorf("leaf itself should resolve: %v", err)
	}
}

// TestStaleCacheDeletedFile: a cache that outlived its media. If the real file a
// leaf points at is deleted, Stat and OpenFile must surface the underlying
// ErrNotExist rather than panic or serve a zero-length file.
func TestStaleCacheDeletedFile(t *testing.T) {
	root := t.TempDir()
	item := mediaItem(t, root, "dl/M.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "M", "2020" })
	f := newTestFS(t, root, []library.LibraryItem{item}, nil)
	ctx := context.Background()
	leaf := "/Movies/M (2020)/M.2020.mkv"

	if _, err := f.Stat(ctx, leaf); err != nil {
		t.Fatalf("leaf missing before delete: %v", err)
	}
	// The media vanishes (moved/deleted) while the cache still references it.
	if err := os.Remove(item.FilePath); err != nil {
		t.Fatalf("remove media: %v", err)
	}
	if _, err := f.Stat(ctx, leaf); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat of deleted media err = %v, want ErrNotExist", err)
	}
	if _, err := f.OpenFile(ctx, leaf, os.O_RDONLY, 0); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OpenFile of deleted media err = %v, want ErrNotExist", err)
	}
}
