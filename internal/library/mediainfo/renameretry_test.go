package mediainfo

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestRenameWithRetryGivesUpOnRealErrors: the retry waits out a transient
// Windows holder, it does NOT paper over an error that will never clear. A
// missing source is permanent, so it must come back immediately rather than
// after burning the whole retry window.
func TestRenameWithRetryGivesUpOnRealErrors(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	err := renameWithRetry(filepath.Join(dir, "does-not-exist"), filepath.Join(dir, "dst"))
	if err == nil {
		t.Fatal("renaming a missing file must fail")
	}
	if elapsed := time.Since(start); elapsed >= renameRetryWindow {
		t.Fatalf("a permanent error was retried for %v; it must return at once", elapsed)
	}
}

// TestRenameWithRetrySucceeds is the ordinary path: nothing holds the
// destination, so the rename lands on the first try.
func TestRenameWithRetrySucceeds(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := renameWithRetry(src, dst); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "payload" {
		t.Fatalf("dst = %q, err %v", got, err)
	}
}

// TestTransientRenameBlockClassification pins what the predicate answers, so a
// future edit cannot quietly widen "wait this out" to cover a permanent fault.
//
// The Windows constants are not reachable from a Linux test, so this checks the
// half that is: on POSIX nothing is transient, because rename(2) replaces the
// destination no matter who holds it. On Windows the build-tagged file supplies
// the real predicate and this expectation is skipped.
func TestTransientRenameBlockClassification(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX expectations; the Windows predicate is exercised on windows-latest")
	}
	for _, err := range []error{
		nil,
		os.ErrPermission,
		os.ErrNotExist,
		syscall.EACCES,
		errors.New("some other failure"),
	} {
		if isTransientRenameBlock(err) {
			t.Fatalf("nothing is a transient rename block on POSIX, got true for %v", err)
		}
	}
}
