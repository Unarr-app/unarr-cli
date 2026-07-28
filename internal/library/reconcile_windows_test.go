//go:build windows

package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Windows counterparts of the POSIX-only tests. chmod on Windows does not strip
// unlink rights from a directory the way POSIX does, so the parent-dir EACCES
// provocation used in reconcile_posix_test.go can't be reproduced here — that arm
// is explicitly skipped with a reason. The portable half (error → guidance
// mapping) IS tested, driven through os.ErrPermission which
// ERROR_ACCESS_DENIED satisfies on Windows.

func TestReconcilePermissionDenied_WindowsSkipped(t *testing.T) {
	t.Skip("windows: chmod does not strip directory unlink rights, so a parent-dir EACCES can't be provoked; the guidance mapping is covered by TestClassifyRemoveError_Windows")
}

// TestClassifyRemoveError_Windows verifies the permission path via os.ErrPermission
// (the portable sentinel ERROR_ACCESS_DENIED maps to), plus the generic fallback.
func TestClassifyRemoveError_Windows(t *testing.T) {
	// A PathError wrapping os.ErrPermission — what os.Remove yields on a
	// Windows access-denied.
	permErr := &os.PathError{Op: "remove", Path: `C:\x`, Err: os.ErrPermission}
	outcome, guidance := classifyRemoveError(`C:\some\path`, permErr)
	if outcome != OutcomePermission {
		t.Errorf("outcome = %v, want OutcomePermission", outcome)
	}
	if !strings.Contains(guidance, "permission denied") {
		t.Errorf("guidance %q missing 'permission denied'", guidance)
	}
	if !strings.Contains(guidance, `C:\some\path`) {
		t.Errorf("guidance should name the path, got %q", guidance)
	}

	// A generic error falls through to OutcomeOther, surfaced verbatim.
	outcome, guidance = classifyRemoveError(`C:\y`, os.ErrClosed)
	if outcome != OutcomeOther {
		t.Errorf("outcome = %v, want OutcomeOther", outcome)
	}
	if !strings.Contains(guidance, `C:\y`) {
		t.Errorf("guidance should name the path, got %q", guidance)
	}
}

// TestReconcileBasicApply_Windows: a minimal apply on Windows exercises the same
// separators and path handling as Linux (native \ separators via filepath).
func TestReconcileBasicApply_Windows(t *testing.T) {
	root := t.TempDir()
	writeSized(t, filepath.Join(root, "stub.mkv"), 512)

	opts := DefaultReconcileOptions()
	opts.Apply = true
	if _, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts); err != nil {
		t.Fatal(err)
	}
	mustGone(t, filepath.Join(root, "stub.mkv"))
}
