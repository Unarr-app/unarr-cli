package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTrayLockAdmitsOneTray: a second tray is turned away while the first holds
// the lock, and admitted once it lets go. Two trays watching one daemon is how a
// field crash report arrived twice, 3.7 s apart.
func TestTrayLockAdmitsOneTray(t *testing.T) {
	isolatePaths(t)

	release, ok := acquireTrayLock()
	if !ok {
		t.Fatal("the first tray could not take a free lock")
	}
	if _, ok := acquireTrayLock(); ok {
		t.Fatal("a second tray took the lock while the first held it: both would mail every crash")
	}
	release()

	again, ok := acquireTrayLock()
	if !ok {
		t.Fatal("the lock was not released: a tray relaunched after Quit would refuse to start")
	}
	again()
}

// TestTrayLockFailureDoesNotBlockTheTray: a lock that cannot even be created
// must not keep the tray from starting — that would trade a duplicate report for
// no tray at all.
func TestTrayLockFailureDoesNotBlockTheTray(t *testing.T) {
	isolatePaths(t)
	// A regular file where a parent directory should be: MkdirAll cannot succeed.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UNARR_CONFIG_DIR", filepath.Join(blocker, "unarr"))

	release, ok := acquireTrayLock()
	if !ok {
		t.Fatal("an uncreatable lock turned the tray away; it must start without one")
	}
	release()
}
