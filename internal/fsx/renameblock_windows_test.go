package fsx

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestIsSharingViolationSeesAHandleThatDoesNotShare holds a file open the way the
// launcher shim's `cmd /c ... >> unarr.boot.log` does — no write sharing — and
// checks that a second open is recognised as that, and only that.
func TestIsSharingViolationSeesAHandleThatDoesNotShare(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unarr.boot.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, windows.FILE_SHARE_READ, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)

	_, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if !IsSharingViolation(err) {
		t.Fatalf("open against a non-sharing writer: IsSharingViolation(%v) = false", err)
	}
	if IsSharingViolation(windows.ERROR_ACCESS_DENIED) {
		t.Error("ERROR_ACCESS_DENIED read as a sharing violation: a permissions problem would pass as a live shim")
	}
}
