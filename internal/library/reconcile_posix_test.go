//go:build !windows

package library

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// These tests exercise POSIX-only behavior: symlink handling and the
// permission-denied / read-only removal paths that rely on chmod semantics
// Windows does not share (chmod there does not strip read/delete rights the same
// way). The Windows equivalents / skips live in reconcile_windows_test.go.

// TestReconcileSymlinkVideoNotFollowed: a symlink whose target is a real video is
// not itself a stub, and reconcile does not chase it out of the tree.
func TestReconcileSymlinkToRealVideo(t *testing.T) {
	root := t.TempDir()
	realDir := t.TempDir()
	realVideo := filepath.Join(realDir, "real.mkv")
	writeSized(t, realVideo, 2*mib)

	link := filepath.Join(root, "link.mkv")
	if err := os.Symlink(realVideo, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	opts := DefaultReconcileOptions()
	opts.Apply = true
	if _, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts); err != nil {
		t.Fatal(err)
	}
	// os.Stat follows the link → sees a >= floor video → not a stub. The link
	// stays; the (out-of-root) target is never touched.
	mustExist(t, link)
	mustExist(t, realVideo)
}

// TestReconcilePermissionDeniedIsGuided: make the PARENT dir non-writable so
// os.Remove of a flagged stub fails with EACCES. Assert (a) no crash, (b) the
// sweep continues to the other flagged file, (c) the summary carries an
// actionable permission-denied guidance line.
func TestReconcilePermissionDeniedIsGuided(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are bypassed, EACCES can't be provoked")
	}
	root := t.TempDir()

	// Locked dir: a stub whose parent is read-only → removal fails. A valid video
	// sits beside it so the dir is NOT video-less (not an empty_dir) — otherwise
	// dropFindingsUnderDirs would absorb the stub into an empty_dir finding and the
	// EACCES would be reported on the DIR's RemoveAll instead of on the file. Here
	// we want the failure attributed to the file itself.
	lockedDir := filepath.Join(root, "locked")
	if err := os.MkdirAll(lockedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(lockedDir, "Feature.mkv"), 2*mib) // real video → dir not empty
	lockedStub := filepath.Join(lockedDir, "stub.mkv")
	writeSized(t, lockedStub, 512)

	// A second, removable stub elsewhere → proves the sweep continues.
	freeStub := filepath.Join(root, "free.mkv")
	writeSized(t, freeStub, 512)

	if err := os.Chmod(lockedDir, 0o500); err != nil { // r-x: can't unlink children
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockedDir, 0o755) }) // let t.TempDir clean up

	opts := DefaultReconcileOptions()
	opts.Apply = true
	_, summary, err := ReconcileWithSummary(ReconcilePaths{DownloadDir: root}, nil, opts)
	if err != nil {
		t.Fatalf("reconcile must not fail fatally on a per-file EACCES: %v", err)
	}

	// (b) the other file WAS removed.
	mustGone(t, freeStub)
	// The locked stub survived.
	mustExist(t, lockedStub)

	// (c) the summary carries a permission failure with guidance.
	var found *RemoveFailure
	for i := range summary.Failures {
		if filepath.Clean(summary.Failures[i].Path) == filepath.Clean(lockedStub) {
			found = &summary.Failures[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected a failure entry for %s, summary=%+v", lockedStub, summary)
	}
	if found.Outcome != OutcomePermission {
		t.Errorf("outcome = %v, want OutcomePermission", found.Outcome)
	}
	if !contains(found.Guidance, "permission denied") {
		t.Errorf("guidance should mention permission denied, got %q", found.Guidance)
	}
	if !contains(found.Guidance, lockedStub) {
		t.Errorf("guidance should name the path, got %q", found.Guidance)
	}
}

// TestClassifyRemoveErrorPOSIX table-drives the errno → outcome/guidance mapping
// for the codes that only carry meaning on POSIX.
func TestClassifyRemoveErrorPOSIX(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		outcome RemoveOutcome
		phrase  string
	}{
		{"EACCES", syscall.EACCES, OutcomePermission, "permission denied"},
		{"EPERM", syscall.EPERM, OutcomePermission, "permission denied"},
		{"EROFS", syscall.EROFS, OutcomeReadOnly, "read-only"},
		{"ENOSPC", syscall.ENOSPC, OutcomeDiskFull, "no space left"},
		{"EIO", syscall.EIO, OutcomeUnreachable, "disconnected"},
		{"ESTALE", syscall.ESTALE, OutcomeUnreachable, "disconnected"},
		{"wrapped EACCES", &os.PathError{Op: "remove", Path: "/x", Err: syscall.EACCES}, OutcomePermission, "permission denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome, guidance := classifyRemoveError("/some/path", tt.err)
			if outcome != tt.outcome {
				t.Errorf("outcome = %v, want %v", outcome, tt.outcome)
			}
			if !contains(guidance, tt.phrase) {
				t.Errorf("guidance %q missing %q", guidance, tt.phrase)
			}
		})
	}
	// Sanity: os.ErrPermission unwraps too (Windows relies on this).
	if !errors.Is(&os.PathError{Err: syscall.EACCES}, os.ErrPermission) {
		t.Error("EACCES should satisfy os.ErrPermission")
	}
}

// TestDiskUsageSparseVsApparent is the core of the honest-freed-bytes fix: a
// SPARSE file (1 GiB apparent, ~0 real blocks) must report diskUsage ≈ 0, and its
// reconcile Finding.Bytes must be the real usage — NOT 1 GiB. A normal file of N
// bytes reports ≈ N (rounded up to a block). POSIX-only: Windows diskUsage falls
// back to apparent size (see reconcile_usage_windows.go).
func TestDiskUsageSparseVsApparent(t *testing.T) {
	root := t.TempDir()

	// Sparse: truncate to 1 GiB apparent with no data written → holes, ~0 blocks.
	const gib = int64(1) << 30
	sparse := filepath.Join(root, "sparse.part")
	f, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(gib); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	sInfo, err := os.Stat(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if sInfo.Size() != gib {
		t.Fatalf("apparent size = %d, want %d", sInfo.Size(), gib)
	}
	// Some filesystems (e.g. tmpfs without hole support) may materialise the
	// truncate; only assert the sparse property where the FS actually gave us holes.
	sparseUsage := diskUsage(sInfo)
	if sparseUsage >= gib {
		t.Skipf("filesystem did not create a sparse file (usage=%d ≈ apparent) — sparse test not meaningful here", sparseUsage)
	}
	if sparseUsage > 16*1024*1024 { // generous ceiling: real blocks must be a tiny fraction of 1 GiB
		t.Errorf("sparse diskUsage = %d, expected ~0 (much less than the 1 GiB apparent)", sparseUsage)
	}

	// Normal file of N bytes → diskUsage ≈ N (>= N is fine; block rounding only
	// ever rounds UP, and must stay within one block of the apparent size).
	const n = 100_000
	normal := filepath.Join(root, "normal.bin")
	writeSized(t, normal, n)
	nInfo, err := os.Stat(normal)
	if err != nil {
		t.Fatal(err)
	}
	nu := diskUsage(nInfo)
	if nu < n {
		t.Errorf("normal diskUsage = %d, want >= %d", nu, n)
	}
	if nu > n+512*8 { // within a few blocks of apparent
		t.Errorf("normal diskUsage = %d, unexpectedly larger than apparent %d", nu, n)
	}
}

// TestReconcileSparseStubFindingBytes: a sparse .part flagged as an orphan partial
// must carry Bytes ≈ 0 (real) and Apparent == 1 GiB — so the "freed" total tells
// the truth about disk space, not the apparent lie ("57.7 GB" that freed ~7 GB).
func TestReconcileSparseStubFindingBytes(t *testing.T) {
	root := t.TempDir()
	const gib = int64(1) << 30
	sparse := filepath.Join(root, "preallocated.part")
	f, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(gib); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	info, err := os.Stat(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if diskUsage(info) >= gib {
		t.Skip("filesystem did not create a sparse file — test not meaningful here")
	}

	findings, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, DefaultReconcileOptions())
	if err != nil {
		t.Fatal(err)
	}
	var got *Finding
	for i := range findings {
		if filepath.Clean(findings[i].Path) == filepath.Clean(sparse) {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("expected an orphan_partial finding for %s, got %+v", sparse, findings)
	}
	if got.Kind != KindOrphanPartial {
		t.Errorf("kind = %s, want orphan_partial", got.Kind)
	}
	// Real bytes freed ≈ 0, NOT the 1 GiB apparent.
	if got.Bytes >= gib {
		t.Errorf("Finding.Bytes = %d, must be the real (near-zero) on-disk usage, not the 1 GiB apparent", got.Bytes)
	}
	if got.Apparent != gib {
		t.Errorf("Finding.Apparent = %d, want %d (the logical size)", got.Apparent, gib)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestApplyFindingsRefusesSymlinkEscape is the #1 regression: a finding whose path
// is a symlink resolving OUTSIDE the configured roots must be refused (parity with
// delete.go's EvalSymlinks confinement), so a symlinked entry cannot be used to
// delete shared/outside data.
func TestApplyFindingsRefusesSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	// A real file outside the roots, and a symlink to it from inside root.
	target := filepath.Join(outside, "precious.mkv")
	writeSized(t, target, 512)
	link := filepath.Join(root, "sneaky.mkv")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// Craft a finding that (lexically) sits inside root but resolves outside.
	findings := []Finding{
		{Path: link, Kind: KindStubVideo, Bytes: 512, IsDir: false},
	}
	summary := applyFindings(findings, []string{filepath.Clean(root)})

	if summary.Removed != 0 {
		t.Errorf("Removed=%d, want 0 (a symlink escaping the roots must be refused)", summary.Removed)
	}
	// The outside target must be untouched.
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the symlink's outside target must survive: %v", err)
	}
}

// TestApplyFindingsAllowsInRootSymlink: a symlink that resolves to a target STILL
// inside the roots is allowed (the removal proceeds) — the guard rejects only
// escapes, not benign in-tree symlinks / mount indirection.
func TestApplyFindingsAllowsInRootSymlink(t *testing.T) {
	root := t.TempDir()
	realTarget := filepath.Join(root, "real.mkv")
	writeSized(t, realTarget, 512)
	link := filepath.Join(root, "alias.mkv")
	if err := os.Symlink(realTarget, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	findings := []Finding{{Path: link, Kind: KindStubVideo, Bytes: 512, IsDir: false}}
	summary := applyFindings(findings, []string{filepath.Clean(root)})

	if summary.Removed != 1 {
		t.Errorf("Removed=%d, want 1 (an in-root symlink target is allowed)", summary.Removed)
	}
}
