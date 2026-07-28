package engine

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
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
	// Must clear the anti-stub floor (minPlausibleVideoBytes): a .mkv below 1 MiB is
	// rejected as a stub, so a "valid file" fixture has to be a plausible video size.
	size := int64(minPlausibleVideoBytes + 1024)
	os.WriteFile(path, make([]byte, size), 0o644)

	err := verify(&Result{FilePath: path, Size: size})
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
	// Above the anti-stub floor so the video passes on its own; Size=0 (unknown)
	// then means only the floor + zero-byte checks apply.
	os.WriteFile(path, make([]byte, minPlausibleVideoBytes+1024), 0o644)

	// Size=0 means unknown, should pass
	err := verify(&Result{FilePath: path, Size: 0})
	if err != nil {
		t.Errorf("no expected size should pass: %v", err)
	}
}

func TestIsStorageStatErr(t *testing.T) {
	// EIO / ESTALE = the mount faulted → storage.
	if !isStorageStatErr(syscall.EIO) {
		t.Error("isStorageStatErr(EIO) = false, want true (I/O error = dropped mount)")
	}
	if !isStorageStatErr(syscall.ESTALE) {
		t.Error("isStorageStatErr(ESTALE) = false, want true (stale NFS handle)")
	}
	// Wrapped, as os.Stat returns them.
	if !isStorageStatErr(&os.PathError{Op: "stat", Path: "/mnt/nas/x", Err: syscall.EIO}) {
		t.Error("isStorageStatErr(*PathError{EIO}) = false, want true")
	}
	// A genuinely missing file (ENOENT) is NOT storage — it stays "file not found".
	if isStorageStatErr(syscall.ENOENT) {
		t.Error("isStorageStatErr(ENOENT) = true — a missing file must NOT read as a storage fault")
	}
	if isStorageStatErr(errors.New("some other error")) {
		t.Error("isStorageStatErr(generic) = true, want false")
	}
}
