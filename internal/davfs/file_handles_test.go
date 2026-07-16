package davfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestDirFileWriteReadOnly: the read-only contract at the directory-handle layer.
// A nonconforming client can call Write directly on a directory handle — it must
// get os.ErrPermission (distinct from the already-tested realFile.Write and the
// OpenFile flag rejection).
func TestDirFileWriteReadOnly(t *testing.T) {
	d := newDirFile(newDir("Movies"))
	n, err := d.Write([]byte("nope"))
	if n != 0 || !errors.Is(err, os.ErrPermission) {
		t.Errorf("dirFile.Write = (%d, %v), want (0, ErrPermission)", n, err)
	}
}

// TestDirFileStat: a directory handle reports its node info.
func TestDirFileStat(t *testing.T) {
	d := newDirFile(newDir("TV Shows"))
	fi, err := d.Stat()
	if err != nil {
		t.Fatalf("dirFile.Stat err = %v", err)
	}
	if !fi.IsDir() || fi.Name() != "TV Shows" {
		t.Errorf("dirFile.Stat = (name=%q, isDir=%v), want (TV Shows, true)", fi.Name(), fi.IsDir())
	}
}

// TestRealFileReaddirNotDir: Readdir on a file handle is a category error (errNotDir),
// not an empty listing.
func TestRealFileReaddirNotDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.mkv")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()

	rf := &realFile{File: fh, name: "m.mkv"}
	if _, err := rf.Readdir(0); !errors.Is(err, errNotDir) {
		t.Errorf("realFile.Readdir err = %v, want errNotDir", err)
	}
}

// TestRealFileStatErrorSurfaced: when the underlying *os.File.Stat() fails (here the
// handle is closed, mimicking a media file unlinked after Open) the error is
// surfaced rather than a bogus nodeInfo returned.
func TestRealFileStatErrorSurfaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone.mkv")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fh.Close() // make the subsequent *os.File.Stat() fail

	rf := &realFile{File: fh, name: "gone.mkv"}
	fi, err := rf.Stat()
	if err == nil {
		t.Errorf("realFile.Stat on a closed handle = (%v, nil), want an error surfaced", fi)
	}
	if fi != nil {
		t.Errorf("realFile.Stat returned non-nil FileInfo on error: %v", fi)
	}
}
