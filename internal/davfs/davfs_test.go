package davfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library"
	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

const dummyBody = "dummy-media-bytes-0123456789"

// mediaItem creates a real file at root/rel and returns a LibraryItem pointing
// at it, after applying mut for the parsed metadata.
func mediaItem(t *testing.T, root, rel string, mut func(*library.LibraryItem)) library.LibraryItem {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(dummyBody), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	it := library.LibraryItem{
		FilePath: p,
		FileName: filepath.Base(p),
		FileSize: int64(len(dummyBody)),
		ModTime:  time.Now().UTC().Format(time.RFC3339),
	}
	if mut != nil {
		mut(&it)
	}
	return it
}

// newTestFS writes the items to a temp library cache and returns an FS over it.
func newTestFS(t *testing.T, cacheDir string, items []library.LibraryItem, allow func(string) bool) *FS {
	t.Helper()
	cachePath := filepath.Join(cacheDir, "library.json")
	if err := library.SaveCacheTo(&library.LibraryCache{Items: items}, cachePath); err != nil {
		t.Fatalf("save cache: %v", err)
	}
	return New(Options{
		Load:            func() (*library.LibraryCache, error) { return library.LoadCacheFrom(cachePath) },
		AllowPath:       allow,
		CachePath:       cachePath,
		RefreshInterval: time.Millisecond,
	})
}

func statDir(t *testing.T, f *FS, name string) os.FileInfo {
	t.Helper()
	fi, err := f.Stat(context.Background(), name)
	if err != nil {
		t.Fatalf("Stat(%q): %v", name, err)
	}
	return fi
}

func listNames(t *testing.T, f *FS, name string) []string {
	t.Helper()
	fh, err := f.OpenFile(context.Background(), name, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile(%q): %v", name, err)
	}
	defer fh.Close()
	infos, err := fh.Readdir(0)
	if err != nil {
		t.Fatalf("Readdir(%q): %v", name, err)
	}
	names := make([]string, len(infos))
	for i, fi := range infos {
		names[i] = fi.Name()
	}
	return names
}

func TestLayoutMoviesAndShows(t *testing.T) {
	root := t.TempDir()
	items := []library.LibraryItem{
		mediaItem(t, root, "dl/Inception.2010.1080p.mkv", func(it *library.LibraryItem) {
			it.Title, it.Year = "Inception", "2010"
		}),
		mediaItem(t, root, "dl/Show.S01E01.1080p.mkv", func(it *library.LibraryItem) {
			it.Title, it.Season, it.Episode = "Show S01E01", 1, 1
		}),
		mediaItem(t, root, "dl/Show.S00E05.mkv", func(it *library.LibraryItem) {
			it.Title, it.Season, it.Episode = "Show S00E05", 0, 5
		}),
	}
	f := newTestFS(t, root, items, nil)

	if !statDir(t, f, "/Movies/Inception (2010)").IsDir() {
		t.Error("movie folder not a directory")
	}
	fi := statDir(t, f, "/Movies/Inception (2010)/Inception.2010.1080p.mkv")
	if fi.IsDir() || fi.Size() != int64(len(dummyBody)) {
		t.Errorf("movie leaf wrong: isDir=%v size=%d", fi.IsDir(), fi.Size())
	}
	if !statDir(t, f, "/TV Shows/Show/Season 01/Show.S01E01.1080p.mkv").Mode().IsRegular() {
		t.Error("episode leaf missing")
	}
	if _, err := f.Stat(context.Background(), "/TV Shows/Show/Specials/Show.S00E05.mkv"); err != nil {
		t.Errorf("specials leaf missing: %v", err)
	}

	if names := listNames(t, f, "/"); len(names) != 2 || names[0] != movieRoot || names[1] != tvRoot {
		t.Errorf("root listing = %v, want [Movies, TV Shows]", names)
	}
}

func TestHiddenItems(t *testing.T) {
	root := t.TempDir()
	items := []library.LibraryItem{
		mediaItem(t, root, "dl/Good.Movie.2021.mkv", func(it *library.LibraryItem) { it.Title = "Good Movie" }),
		mediaItem(t, root, "dl/Damaged.2021.mkv", func(it *library.LibraryItem) {
			it.Title = "Damaged"
			it.MediaInfo = &mediainfo.MediaInfo{Integrity: &mediainfo.IntegrityInfo{Damaged: true, Reason: "truncated"}}
		}),
		mediaItem(t, root, "dl/Some.Movie.sample.mkv", func(it *library.LibraryItem) { it.Title = "Some Movie sample" }),
		{FilePath: "", FileName: "ghost.mkv", Title: "Ghost", ScanError: "stat failed"},
	}
	f := newTestFS(t, root, items, nil)

	names := listNames(t, f, "/Movies")
	if len(names) != 1 || names[0] != "Good Movie" {
		t.Errorf("Movies = %v, want only [Good Movie] (damaged/sample/errored hidden)", names)
	}
}

func TestReadOnlyMutations(t *testing.T) {
	root := t.TempDir()
	f := newTestFS(t, root, []library.LibraryItem{
		mediaItem(t, root, "dl/M.2020.mkv", func(it *library.LibraryItem) { it.Title = "M" }),
	}, nil)
	ctx := context.Background()

	if err := f.Mkdir(ctx, "/new", 0o755); !errors.Is(err, os.ErrPermission) {
		t.Errorf("Mkdir err = %v, want ErrPermission", err)
	}
	if err := f.RemoveAll(ctx, "/Movies"); !errors.Is(err, os.ErrPermission) {
		t.Errorf("RemoveAll err = %v, want ErrPermission", err)
	}
	if err := f.Rename(ctx, "/a", "/b"); !errors.Is(err, os.ErrPermission) {
		t.Errorf("Rename err = %v, want ErrPermission", err)
	}
	for _, flag := range []int{os.O_CREATE | os.O_WRONLY, os.O_WRONLY, os.O_RDWR, os.O_TRUNC, os.O_APPEND} {
		if _, err := f.OpenFile(ctx, "/Movies/M (2020)/x.mkv", flag, 0o644); !errors.Is(err, os.ErrPermission) {
			t.Errorf("OpenFile(flag=%d) err = %v, want ErrPermission", flag, err)
		}
	}
}

func TestPathTraversalRejected(t *testing.T) {
	root := t.TempDir()
	f := newTestFS(t, root, []library.LibraryItem{
		mediaItem(t, root, "dl/M.2020.mkv", func(it *library.LibraryItem) { it.Title = "M" }),
	}, nil)
	ctx := context.Background()

	for _, name := range []string{"/../etc/passwd", "/Movies/../../etc", "/..", "/Movies/M (2020)/../../../secret"} {
		if _, err := f.Stat(ctx, name); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Stat(%q) err = %v, want ErrNotExist", name, err)
		}
	}
}

func TestAllowPathPoisonedCache(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() // a real file OUTSIDE the allowed root
	poison := mediaItem(t, outside, "escaped.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "Escaped", "2020" })
	good := mediaItem(t, root, "dl/Legit.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "Legit", "2020" })

	allow := func(p string) bool {
		rel, err := filepath.Rel(root, p)
		return err == nil && rel != ".." && !hasDotDotPrefix(rel)
	}
	f := newTestFS(t, root, []library.LibraryItem{good, poison}, allow)
	ctx := context.Background()

	if _, err := f.OpenFile(ctx, "/Movies/Escaped (2020)/escaped.2020.mkv", os.O_RDONLY, 0); !errors.Is(err, os.ErrPermission) {
		t.Errorf("opening out-of-root leaf err = %v, want ErrPermission", err)
	}
	fh, err := f.OpenFile(ctx, "/Movies/Legit (2020)/Legit.2020.mkv", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("opening in-root leaf: %v", err)
	}
	fh.Close()
}

func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 2 && rel[0] == '.' && rel[1] == '.'
}

// TestStatAllowPathPoisonedCache: Stat of an out-of-root leaf must not leak its
// existence/size/mtime — it applies AllowPath exactly like OpenFile and reports
// ErrNotExist (defense-in-depth vs a poisoned library.json).
func TestStatAllowPathPoisonedCache(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	poison := mediaItem(t, outside, "escaped.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "Escaped", "2020" })
	good := mediaItem(t, root, "dl/Legit.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "Legit", "2020" })

	allow := func(p string) bool {
		rel, err := filepath.Rel(root, p)
		return err == nil && rel != ".." && !hasDotDotPrefix(rel)
	}
	f := newTestFS(t, root, []library.LibraryItem{good, poison}, allow)
	ctx := context.Background()

	if _, err := f.Stat(ctx, "/Movies/Escaped (2020)/escaped.2020.mkv"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat out-of-root leaf err = %v, want ErrNotExist (no metadata leak)", err)
	}
	if _, err := f.Stat(ctx, "/Movies/Legit (2020)/Legit.2020.mkv"); err != nil {
		t.Errorf("Stat in-root leaf: %v", err)
	}
}

func TestCollisionSuffix(t *testing.T) {
	root := t.TempDir()
	// Two distinct real files that map to the same virtual folder + leaf name.
	items := []library.LibraryItem{
		mediaItem(t, root, "a/Movie.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "Movie", "2020" }),
		mediaItem(t, root, "b/Movie.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "Movie", "2020" }),
	}
	f := newTestFS(t, root, items, nil)

	names := listNames(t, f, "/Movies/Movie (2020)")
	if len(names) != 2 {
		t.Fatalf("folder has %d entries, want 2: %v", len(names), names)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	if !got["Movie.2020.mkv"] || !got["Movie.2020 (2).mkv"] {
		t.Errorf("collision names = %v, want both Movie.2020.mkv and Movie.2020 (2).mkv", names)
	}
}

func TestReaddirPagination(t *testing.T) {
	root := t.TempDir()
	items := []library.LibraryItem{
		mediaItem(t, root, "dl/A.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "A", "2020" }),
		mediaItem(t, root, "dl/B.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "B", "2020" }),
		mediaItem(t, root, "dl/C.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "C", "2020" }),
	}
	f := newTestFS(t, root, items, nil)

	fh, err := f.OpenFile(context.Background(), "/Movies", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open Movies: %v", err)
	}
	defer fh.Close()

	first, err := fh.Readdir(2)
	if err != nil || len(first) != 2 {
		t.Fatalf("Readdir(2) = %d entries, err=%v; want 2", len(first), err)
	}
	second, err := fh.Readdir(2)
	if err != nil || len(second) != 1 {
		t.Fatalf("Readdir(2) #2 = %d entries, err=%v; want 1", len(second), err)
	}
	if _, err := fh.Readdir(2); !errors.Is(err, io.EOF) {
		t.Errorf("Readdir past end err = %v, want io.EOF", err)
	}
}

func TestRealFileReadSeek(t *testing.T) {
	root := t.TempDir()
	f := newTestFS(t, root, []library.LibraryItem{
		mediaItem(t, root, "dl/M.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "M", "2020" }),
	}, nil)

	fh, err := f.OpenFile(context.Background(), "/Movies/M (2020)/M.2020.mkv", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer fh.Close()

	if _, err := fh.Seek(6, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(fh, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != dummyBody[6:11] {
		t.Errorf("range read = %q, want %q", buf, dummyBody[6:11])
	}
	// Writes are rejected even on a real-file handle.
	if _, err := fh.Write([]byte("x")); !errors.Is(err, os.ErrPermission) {
		t.Errorf("Write err = %v, want ErrPermission", err)
	}
	if fi, _ := fh.Stat(); fi == nil || fi.Name() != "M.2020.mkv" {
		t.Errorf("Stat name mismatch: %+v", fi)
	}
}

func TestEmptyAndMissingCache(t *testing.T) {
	// Missing cache file → empty tree, root still a browsable directory.
	dir := t.TempDir()
	f := New(Options{
		Load:      library.LoadCache,
		CachePath: filepath.Join(dir, "does-not-exist.json"),
	})
	fi, err := f.Stat(context.Background(), "/")
	if err != nil || !fi.IsDir() {
		t.Fatalf("root Stat on empty FS: fi=%+v err=%v", fi, err)
	}
	if names := listNames(t, f, "/"); len(names) != 0 {
		t.Errorf("empty FS root listing = %v, want []", names)
	}
	if _, err := f.Stat(context.Background(), "/Movies/anything.mkv"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing leaf err = %v, want ErrNotExist", err)
	}
}

func TestDirFileSeekReset(t *testing.T) {
	root := t.TempDir()
	f := newTestFS(t, root, []library.LibraryItem{
		mediaItem(t, root, "dl/M.2020.mkv", func(it *library.LibraryItem) { it.Title, it.Year = "M", "2020" }),
	}, nil)
	fh, err := f.OpenFile(context.Background(), "/", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer fh.Close()
	if _, err := fh.Seek(0, io.SeekStart); err != nil {
		t.Errorf("dir Seek(0,Start) err = %v, want nil", err)
	}
	if _, err := fh.Seek(5, io.SeekStart); err == nil {
		t.Error("dir Seek(5,Start) err = nil, want error")
	}
	if _, err := fh.Read(make([]byte, 1)); err == nil {
		t.Error("dir Read err = nil, want error")
	}
}
