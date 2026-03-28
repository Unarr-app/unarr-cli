package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyNilResult(t *testing.T) {
	if err := verify(nil); err == nil {
		t.Error("expected error for nil result")
	}
}

func TestVerifyEmptyPath(t *testing.T) {
	if err := verify(&Result{}); err == nil {
		t.Error("expected error for empty path")
	}
}

func TestVerifyMissingFile(t *testing.T) {
	err := verify(&Result{FilePath: "/nonexistent/file.mkv"})
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestVerifyEmptyFile(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "empty.mkv")
	os.WriteFile(path, []byte{}, 0o644)

	err := verify(&Result{FilePath: path})
	if err == nil {
		t.Error("expected error for empty file")
	}
}

func TestVerifyValidFile(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "movie.mkv")
	os.WriteFile(path, make([]byte, 1024), 0o644)

	err := verify(&Result{FilePath: path, Size: 1024})
	if err != nil {
		t.Errorf("valid file should pass: %v", err)
	}
}

func TestVerifySizeMismatch(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "movie.mkv")
	os.WriteFile(path, make([]byte, 500), 0o644)

	err := verify(&Result{FilePath: path, Size: 1000})
	if err == nil {
		t.Error("expected error for size mismatch")
	}
}

func TestVerifyNoExpectedSize(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "movie.mkv")
	os.WriteFile(path, make([]byte, 1024), 0o644)

	// Size=0 means unknown, should pass
	err := verify(&Result{FilePath: path, Size: 0})
	if err != nil {
		t.Errorf("no expected size should pass: %v", err)
	}
}
