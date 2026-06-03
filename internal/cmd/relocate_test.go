package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func mkfile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRelocateUnreachable(t *testing.T) {
	root := t.TempDir()
	// A 3-segment-deep file under the current root.
	mkfile(t, filepath.Join(root, "Acme Show", "Season 01", "ep.mkv"))
	// A 2-segment-deep file (too shallow to be matched by a short tail).
	mkfile(t, filepath.Join(root, "Season 01", "lonely.mkv"))

	roots := []string{root}

	// Base-path change: an old-root path whose 3-seg tail exists under the new
	// root → relocates to the real file.
	got := relocateUnreachable("/old/base/Acme Show/Season 01/ep.mkv", roots)
	want := filepath.Join(root, "Acme Show", "Season 01", "ep.mkv")
	if got != want {
		t.Errorf("relocate moved file: got %q want %q", got, want)
	}

	// Only a 2-segment tail would match → must NOT relocate (ambiguous).
	if got := relocateUnreachable("/old/Season 01/lonely.mkv", roots); got != "" {
		t.Errorf("2-segment tail should not match, got %q", got)
	}

	// Nonexistent file → no relocation.
	if got := relocateUnreachable("/old/base/Acme Show/Season 01/missing.mkv", roots); got != "" {
		t.Errorf("missing file should not relocate, got %q", got)
	}

	// Traversal attempt: ".." segments are cleaned by filepath.Join and the
	// result is re-validated, so it can't escape.
	if got := relocateUnreachable("/old/../../../etc/passwd", roots); got != "" {
		t.Errorf("traversal should not match, got %q", got)
	}
}

func TestRelocateUnreachableSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	// A real file living OUTSIDE any allowed root.
	mkfile(t, filepath.Join(outside, "sub", "secret.mkv"))
	// A symlink inside the root pointing at the outside tree.
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// The lexical candidate root/link/sub/secret.mkv exists (os.Stat follows the
	// symlink), but after resolving symlinks it's outside the root → must be
	// rejected so the stream can't escape the allowed dirs.
	got := relocateUnreachable("/old/link/sub/secret.mkv", []string{root})
	if got != "" {
		t.Errorf("symlink escape must be rejected, got %q", got)
	}
}
