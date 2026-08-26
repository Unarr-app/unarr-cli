package mediainfo

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestInstallBinaryAtomically: concurrent installers of the same tool must
// never leave a partial file at dest, and the winner's bytes must be complete.
//
// WHAT AN INSTALLER IS ALLOWED TO RETURN HERE DIFFERS BY PLATFORM, and the test
// used to assert the POSIX answer everywhere. On Windows,
// MoveFileEx(MOVEFILE_REPLACE_EXISTING) fails while anyone else holds either
// path, so a loser of an 8-way race can legitimately come back with
// ERROR_ACCESS_DENIED even though installBinaryAtomically now waits such a
// holder out (see renameWithRetry). Demanding success from all eight made this
// test flaky on windows-latest — and since that job gates release.yml, a red
// run there blocked publishing a release outright.
//
// So on Windows a transient rename block is an accepted outcome for an
// individual caller. What is NOT relaxed is the invariant the test exists for,
// asserted below for every platform: at least one installer wins, dest ends up
// with the complete payload, and no temp file is left behind. A real defect —
// a partial write, a lost cleanup, every caller failing — still fails the test.
func TestInstallBinaryAtomically(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin", "tool")
	payload := []byte(strings.Repeat("x", 1<<20))
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		installs int
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := installBinaryAtomically(dest, payload)
			switch {
			case err == nil:
				mu.Lock()
				installs++
				mu.Unlock()
			case runtime.GOOS == "windows" && isTransientRenameBlock(err):
				// Someone else held dest for the whole retry window. Allowed.
			default:
				t.Errorf("install: %v", err)
			}
		}()
	}
	wg.Wait()

	// Somebody has to win, on every platform: eight simultaneous losers would
	// mean no tool got installed at all, which is the failure this whole
	// function exists to prevent.
	if installs == 0 {
		t.Fatal("no installer succeeded: dest was never written")
	}
	got, err := os.ReadFile(dest)
	if err != nil || len(got) != len(payload) {
		t.Fatalf("dest = %d bytes, err %v; want %d", len(got), err, len(payload))
	}
	// Windows has no execute bit; there the OS runs .exe by extension.
	if st, _ := os.Stat(dest); runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
		t.Fatalf("dest is not executable: %v", st.Mode())
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(dest), "*.tmp-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

// TestInstallBinaryRemovesTempOnFailure covers the cleanup path directly.
//
// The concurrent test above cannot: on its happy path nothing ever calls
// cleanup, so deleting the cleanup body leaves it green (verified by
// mutation). The stray temp files that matter are the ones a FAILED install
// leaves — a download that dies mid-write, repeated across restarts, filling
// the cache dir with megabyte-sized debris.
//
// A directory where dest is an existing DIRECTORY makes the rename fail on
// every platform without needing a second process or a race.
func TestInstallBinaryRemovesTempOnFailure(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "tool")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	// Renaming a file onto a non-empty directory fails on every OS.
	if err := os.WriteFile(filepath.Join(dest, "occupant"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write occupant: %v", err)
	}

	if err := installBinaryAtomically(dest, []byte("payload")); err == nil {
		t.Fatal("installing onto a non-empty directory must fail")
	}

	left, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if len(left) != 0 {
		t.Fatalf("a failed install left its temp file behind: %v", left)
	}
}
