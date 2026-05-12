package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirSize(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.bin"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.bin"), make([]byte, 250), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := DirSize(root)
	if err != nil {
		t.Fatalf("DirSize error: %v", err)
	}
	if got != 350 {
		t.Errorf("DirSize = %d, want 350", got)
	}
}

func TestDirSizeEmpty(t *testing.T) {
	got, err := DirSize(t.TempDir())
	if err != nil {
		t.Fatalf("DirSize empty dir error: %v", err)
	}
	if got != 0 {
		t.Errorf("DirSize empty = %d, want 0", got)
	}
}

func TestDirSizeMissing(t *testing.T) {
	// Walk skips unreadable entries — missing path returns 0 with no error.
	got, err := DirSize("/nonexistent/path/zzz")
	if err != nil {
		t.Errorf("DirSize on missing path = err %v, want nil", err)
	}
	if got != 0 {
		t.Errorf("DirSize on missing path = %d, want 0", got)
	}
}

func TestDiskInfoCurrentDir(t *testing.T) {
	free, total, err := DiskInfo(".")
	if err != nil {
		t.Fatalf("DiskInfo: %v", err)
	}
	if total <= 0 {
		t.Errorf("total bytes should be > 0, got %d", total)
	}
	if free > total {
		t.Errorf("free (%d) should not exceed total (%d)", free, total)
	}
}
