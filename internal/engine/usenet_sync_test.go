package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSyncTreeFile fsyncs a single delivered file without error.
func TestSyncTreeFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(p, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := syncTree(p); err != nil {
		t.Errorf("syncTree(file) = %v, want nil", err)
	}
}

// TestSyncTreeDir fsyncs every regular file in a multi-file release directory,
// skipping subdirectories, without error.
func TestSyncTreeDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"e01.mkv", "e02.mkv", "sub/e03.mkv"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := syncTree(dir); err != nil {
		t.Errorf("syncTree(dir) = %v, want nil", err)
	}
}

// TestSyncTreeMissing surfaces a stat error for a path that does not exist
// (a failed write-back must fail the download, not be swallowed).
func TestSyncTreeMissing(t *testing.T) {
	if err := syncTree(filepath.Join(t.TempDir(), "nope.mkv")); err == nil {
		t.Error("syncTree(missing) = nil, want error")
	}
}
