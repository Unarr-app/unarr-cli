package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestLog returns a log path inside a fresh temp dir.
func newTestLog(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "unarr.log")
}

// mustRead returns a file's contents, or "" when it does not exist.
func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// unrotatableLog seeds an over-budget log file that no rotation can truncate:
// the reproducible Linux stand-in for a Windows holder such as cmd.exe's `>>`
// redirect, which grants only FILE_SHARE_READ and makes os.Truncate's
// GENERIC_WRITE open a sharing violation.
func unrotatableLog(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions, so the truncate cannot be blocked")
	}
	path := filepath.Join(t.TempDir(), "unarr.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("o", 2*1024*1024)), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	return path
}

// TestRotateNowUnderAPosixAppendHolder covers ONLY the POSIX shape: a foreign
// holder (launchd, the detached launcher) that opened the log with a real
// O_APPEND descriptor, so the kernel recomputes the write offset from the file
// length every time. That premise is what makes copy-truncate exact, and it is
// what a file the daemon owns no longer needs — see Writer.
func TestRotateNowUnderAPosixAppendHolder(t *testing.T) {
	path := newTestLog(t)
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	defer holder.Close()
	if _, err := holder.Write([]byte(strings.Repeat("o", 2*1024*1024))); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := RotateNow(Options{Path: path, MaxSizeMB: 1, MaxFiles: 2}); err != nil {
		t.Fatalf("RotateNow: %v", err)
	}

	// The live file must be the SAME file, emptied — not a new inode, which is
	// what a rename would leave the holder writing past.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat live: %v", err)
	}
	if fi.Size() != 0 {
		t.Fatalf("live log is %d bytes after rotation, want 0", fi.Size())
	}
	if got := len(mustRead(t, RotatedPath(path, 1))); got != 2*1024*1024 {
		t.Fatalf("unarr.log.1 is %d bytes, want the 2 MiB that was live", got)
	}

	// The append premise itself: the holder's next write lands at offset 0.
	if _, err := holder.Write([]byte("after")); err != nil {
		t.Fatalf("append after rotation: %v", err)
	}
	if got := mustRead(t, path); got != "after" {
		t.Fatalf("live log is %q, want the post-rotation append only", got)
	}
}

// TestRotateNowUnderANonAppendHolder pins what copy-truncate does when the
// premise above does NOT hold: a holder that keeps its own file offset (opened
// without O_APPEND) writes straight back to its pre-truncate offset, so the
// truncate reclaims nothing durable — the file re-grows to its old length with
// a hole in front and the ring stops being bounded.
//
// A characterisation test, not an endorsement. It no longer stands in for
// Windows: cmd.exe's `>>` redirect has since been MEASURED on the VM harness
// and it does not even get this far — it denies write access outright, so the
// truncate fails and RotateNow turns that into a clean no-op
// (TestRotateNowRefusesCleanlyWhenTheHolderDeniesWrites). Windows is served by
// the daemon owning its log and rotating by rename instead. What remains here
// is the general warning for any future holder that appends without O_APPEND.
func TestRotateNowUnderANonAppendHolder(t *testing.T) {
	path := newTestLog(t)
	const seeded = 2 * 1024 * 1024
	// No O_APPEND: this descriptor tracks its own offset.
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	defer holder.Close()
	if _, err := holder.Write([]byte(strings.Repeat("o", seeded))); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := RotateNow(Options{Path: path, MaxSizeMB: 1, MaxFiles: 2}); err != nil {
		t.Fatalf("RotateNow: %v", err)
	}
	// The rotated copy and the truncate itself are correct regardless of the
	// holder — only what happens on the NEXT write differs.
	if got := len(mustRead(t, RotatedPath(path, 1))); got != seeded {
		t.Fatalf("unarr.log.1 is %d bytes, want the %d that was live", got, seeded)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat live: %v", err)
	}
	if fi.Size() != 0 {
		t.Fatalf("live log is %d bytes right after the truncate, want 0", fi.Size())
	}

	if _, err := holder.Write([]byte("after")); err != nil {
		t.Fatalf("write after rotation: %v", err)
	}
	fi, err = os.Stat(path)
	if err != nil {
		t.Fatalf("stat live: %v", err)
	}
	// The whole point: a five-byte write re-inflated the file to its old size,
	// because the holder wrote at offset `seeded`, not at 0.
	if want := int64(seeded + len("after")); fi.Size() != want {
		t.Fatalf("live log is %d bytes after a 5-byte write, want %d — "+
			"a non-append holder is expected to write back at its own offset. "+
			"If this now reports 5, the platform gave the holder append "+
			"semantics and the RotateNow doc comment needs updating",
			fi.Size(), want)
	}
}

// TestRotateNowRefusesCleanlyWhenTheHolderDeniesWrites pins the MESSAGE a
// refused rotation carries: `unarr logs rotate` prints it, and the user needs
// to know what to DO, not just that an open failed. That the ring survives is
// TestRotateNowLeavesAFullRingIntactWhenTheHolderDeniesWrites' job.
func TestRotateNowRefusesCleanlyWhenTheHolderDeniesWrites(t *testing.T) {
	path := unrotatableLog(t)

	err := RotateNow(Options{Path: path, MaxSizeMB: 1, MaxFiles: 3})
	if err == nil {
		t.Fatal("RotateNow reported success on a log it cannot truncate")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "holds it") {
		t.Fatalf("error is not actionable: %v", err)
	}
}

// TestRotateNowKeepsTheRingBoundedAcrossManyRotations exercises the live
// production shape end to end: OpenFile hands out the appending descriptor the
// daemon inherits, and RotateNow (what Sweep calls on a ticker) rotates from
// the OUTSIDE while that descriptor stays open. The ring must stop growing.
func TestRotateNowKeepsTheRingBoundedAcrossManyRotations(t *testing.T) {
	path := newTestLog(t)
	opts := Options{Path: path, MaxSizeMB: 1, MaxFiles: 2}
	f, err := OpenFile(opts)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	const max = 1 << 20
	chunk := []byte(strings.Repeat("x", 128*1024))
	// 10 budgets' worth, so the ring has to drop slots repeatedly.
	for i := 0; i < 80; i++ {
		if _, err := f.Write(chunk); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if err := RotateNow(opts); err != nil {
			t.Fatalf("rotate after write %d: %v", i, err)
		}
	}

	var total int64
	for _, p := range append([]string{path}, RotatedPaths(path, 2)...) {
		fi, serr := os.Stat(p)
		if serr != nil {
			continue
		}
		total += fi.Size()
	}
	// (keep+1) budgets is the ceiling, plus the one oversized chunk that may sit
	// in the live file before the next sweep trims it.
	if ceiling := int64(3*max + len(chunk)); total > ceiling {
		t.Fatalf("ring holds %d bytes, ceiling is %d — rotation is not bounding it", total, ceiling)
	}
	// Retention: nothing may survive past the configured slot count.
	if _, err := os.Stat(RotatedPath(path, 3)); !os.IsNotExist(err) {
		t.Fatalf("unarr.log.3 exists with MaxFiles = 2 (stat err: %v)", err)
	}
}

func TestRotateNowIsANoOpBelowBudgetOrWhenDisabled(t *testing.T) {
	path := newTestLog(t)
	if err := os.WriteFile(path, []byte("small"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, opts := range []Options{
		{Path: path, MaxSizeMB: 1}, // under budget
		{Path: path, MaxSizeMB: 0}, // rotation disabled
		{Path: "", MaxSizeMB: 1},   // no path
	} {
		if err := RotateNow(opts); err != nil {
			t.Fatalf("RotateNow(%+v): %v", opts, err)
		}
	}
	if got := mustRead(t, path); got != "small" {
		t.Fatalf("live log is %q, want it untouched", got)
	}
	if _, err := os.Stat(RotatedPath(path, 1)); !os.IsNotExist(err) {
		t.Fatal("nothing should have been rotated")
	}
}

func TestRotateNowOnAMissingFileIsNotAnError(t *testing.T) {
	if err := RotateNow(Options{Path: newTestLog(t), MaxSizeMB: 1}); err != nil {
		t.Fatalf("a log that was never written is not a failure: %v", err)
	}
}

func TestOpenFileRotatesBeforeHandingOverTheDescriptor(t *testing.T) {
	path := newTestLog(t)
	if err := os.WriteFile(path, []byte(strings.Repeat("o", 2*1024*1024)), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	f, err := OpenFile(Options{Path: path, MaxSizeMB: 1, MaxFiles: 2})
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("fresh")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := mustRead(t, path); got != "fresh" {
		t.Fatalf("live log is %q, want only what was written after the rotation", got)
	}
	if got := len(mustRead(t, RotatedPath(path, 1))); got != 2*1024*1024 {
		t.Fatalf("unarr.log.1 is %d bytes, want the rotated 2 MiB", got)
	}
}

func TestOpenFileCreatesTheDataDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "unarr.log")
	f, err := OpenFile(Options{Path: path, MaxSizeMB: 20})
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	f.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("log file not created: %v", err)
	}
}

func TestRotatedPathsDefaultsWhenUnset(t *testing.T) {
	got := RotatedPaths("/x/unarr.log", 0)
	if len(got) != DefaultMaxFiles {
		t.Fatalf("got %d slots, want the %d default", len(got), DefaultMaxFiles)
	}
	if got[0] != "/x/unarr.log.1" {
		t.Fatalf("first slot is %q, want /x/unarr.log.1", got[0])
	}
}
