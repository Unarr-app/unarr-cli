package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckDiskSpace_Enough(t *testing.T) {
	// A tiny need in a real temp dir (huge free space) → nil.
	if err := CheckDiskSpace(t.TempDir(), 1024, 0); err != nil {
		t.Errorf("expected nil for a 1 KiB need, got %v", err)
	}
}

func TestCheckDiskSpace_Insufficient(t *testing.T) {
	// Need more than any real disk has → InsufficientDiskError.
	err := CheckDiskSpace(t.TempDir(), 1<<62, 0)
	if err == nil {
		t.Fatal("expected an error for an impossibly large need")
	}
	if !IsInsufficientDisk(err) {
		t.Errorf("IsInsufficientDisk = false, want true (err=%v)", err)
	}
	if !strings.Contains(err.Error(), "insufficient disk space") {
		t.Errorf("error message = %q, want it to mention insufficient disk space", err.Error())
	}
}

func TestCheckDiskSpace_ReserveTriggers(t *testing.T) {
	// Tiny need but an impossibly large reserve → free-need < reserve → error.
	err := CheckDiskSpace(t.TempDir(), 1024, 1<<62)
	if !IsInsufficientDisk(err) {
		t.Errorf("expected insufficient when reserve exceeds free space, got %v", err)
	}
}

func TestCheckDiskSpace_UnknownSize(t *testing.T) {
	// need <= 0 means the size is unknown — the check must be skipped, even with
	// an enormous reserve.
	if err := CheckDiskSpace(t.TempDir(), 0, 1<<62); err != nil {
		t.Errorf("need=0 must skip the check, got %v", err)
	}
	if err := CheckDiskSpace(t.TempDir(), -5, 1<<62); err != nil {
		t.Errorf("negative need must skip the check, got %v", err)
	}
}

func TestCheckDiskSpace_BadDirIsBestEffort(t *testing.T) {
	// An unstat-able path → DiskInfo errors → best-effort nil (never block a
	// download on a guard we can't evaluate; ENOSPC stays the backstop).
	bad := filepath.Join(t.TempDir(), "does", "not", "exist")
	if err := CheckDiskSpace(bad, 1<<40, 0); err != nil {
		t.Errorf("unstat-able dir must skip the check, got %v", err)
	}
}
