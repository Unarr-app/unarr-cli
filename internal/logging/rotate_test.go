package logging

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedRing fills the rotated slots with recognisable content, so a test can
// tell "the ring never moved" from "the ring moved and happened to look busy".
// The bug this whole file is about was INVISIBLE to a test that started from an
// empty ring: shifting nothing over nothing leaves nothing, and the assertion
// "slot 1 does not exist" passed while the real history was being destroyed one
// slot per rotation.
func seedRing(t *testing.T, path string, keep int) []string {
	t.Helper()
	want := make([]string, keep+1)
	for i := 1; i <= keep; i++ {
		want[i] = "history slot " + string(rune('0'+i))
		if err := os.WriteFile(RotatedPath(path, i), []byte(want[i]), 0o644); err != nil {
			t.Fatalf("seed slot %d: %v", i, err)
		}
	}
	return want
}

// assertRingIntact fails with the whole picture when any slot moved.
func assertRingIntact(t *testing.T, path string, want []string) {
	t.Helper()
	for i := 1; i < len(want); i++ {
		if got := mustRead(t, RotatedPath(path, i)); got != want[i] {
			t.Fatalf("unarr.log.%d holds %q, want %q — a rotation that could not "+
				"complete moved the ring anyway, which is how the history was lost",
				i, got, want[i])
		}
	}
}

// blockStaging occupies the staging name with a NON-EMPTY DIRECTORY, so every
// way of putting a file there fails: os.Remove refuses (ENOTEMPTY), os.Rename
// onto it refuses, os.OpenFile on it refuses. It is the portable stand-in for
// the measured Windows failure — a live file another process holds without
// FILE_SHARE_DELETE, which MoveFileEx will not move — and, unlike an unwritable
// directory, it blocks ONLY the live-file step: the ring's own renames still
// work, so a shift-first implementation really does destroy history here.
func blockStaging(t *testing.T, path string) {
	t.Helper()
	staging := path + stagingSuffix
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("block staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "occupant"), []byte("x"), 0o644); err != nil {
		t.Fatalf("block staging: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(staging) })
}

// TestRotateThroughStagingKeepsTheRingWhenTheLiveOperationFails is the
// class-level test: it goes straight at the shared primitive with an op that
// fails, and it is what both HIGH-1 and the original copy-truncate bug reduce
// to. Any implementation that shifts the ring before knowing the live-file
// operation worked fails this, whichever path it was written for.
func TestRotateThroughStagingKeepsTheRingWhenTheLiveOperationFails(t *testing.T) {
	path := newTestLog(t)
	if err := os.WriteFile(path, []byte("live"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := seedRing(t, path, 3)

	boom := errors.New("the OS refused to move the live file")
	err := rotateThroughStaging(path, 3, func(string, string) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("rotateThroughStaging returned %v, want the op's own error", err)
	}
	assertRingIntact(t, path, want)
	if got := mustRead(t, path); got != "live" {
		t.Fatalf("live log is %q, want it untouched", got)
	}
	if _, serr := os.Stat(path + stagingSuffix); !os.IsNotExist(serr) {
		t.Fatalf("a staging file survived a failed rotation (stat err: %v)", serr)
	}
}

// TestWriterLeavesAFullRingIntactWhenTheRenameFails is HIGH-1 itself, with the
// ring FULL — the state the existing tests never set up, which is exactly why
// they could not see it. On Windows `unarr logs -f` (the command `daemon
// install` prints) holds the live file without FILE_SHARE_DELETE, so the rename
// fails; the old order dropped the oldest slot and shifted the rest on EVERY
// attempt, so three budgets of logging emptied the whole history while the live
// file kept growing, and nothing could break the loop — the follower only lets
// go when it sees a rotation.
func TestWriterLeavesAFullRingIntactWhenTheRenameFails(t *testing.T) {
	path := newTestLog(t)
	want := seedRing(t, path, 3)
	blockStaging(t, path)

	w, err := NewWriter(Options{Path: path, MaxSizeMB: 1, MaxFiles: 3})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	restoreErr := captureStderr(t)
	// Three budgets' worth: one rotation attempt per budget, enough to have
	// emptied all three slots under the old order.
	chunk := []byte(strings.Repeat("x", 64*1024))
	for i := 0; i < 3*(1<<20)/len(chunk); i++ {
		if _, werr := w.Write(chunk); werr != nil {
			t.Fatalf("a failed rotation must not fail the write: %v", werr)
		}
	}
	stderr := restoreErr()

	assertRingIntact(t, path, want)
	if got := strings.Count(stderr, "unarr:"); got != 1 {
		t.Fatalf("stderr carries %d rotation complaints, want exactly 1:\n%s", got, stderr)
	}
}

// TestRotateNowLeavesAFullRingIntactWhenTheCopyFails is the same shape on the
// outsider's path: the log is perfectly writable, so the cheap probe waves it
// through, and the copy is what fails. The ring must not have moved.
func TestRotateNowLeavesAFullRingIntactWhenTheCopyFails(t *testing.T) {
	path := newTestLog(t)
	const seeded = 2 * 1024 * 1024
	if err := os.WriteFile(path, []byte(strings.Repeat("o", seeded)), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := seedRing(t, path, 3)
	blockStaging(t, path)

	err := RotateNow(Options{Path: path, MaxSizeMB: 1, MaxFiles: 3})
	if err == nil {
		t.Fatal("RotateNow reported success on a rotation that could not be staged")
	}
	assertRingIntact(t, path, want)
	if got := mustStatSize(t, path); got != seeded {
		t.Fatalf("live log is %d bytes, want it untouched", got)
	}
}

// TestRotateNowLeavesAFullRingIntactWhenTheHolderDeniesWrites is the measured
// Windows holder (cmd.exe's `>>`, reproduced with a read-only file) meeting a
// FULL ring: the probe rejects it, and rejecting must cost nothing.
func TestRotateNowLeavesAFullRingIntactWhenTheHolderDeniesWrites(t *testing.T) {
	path := unrotatableLog(t)
	want := seedRing(t, path, 3)

	if err := RotateNow(Options{Path: path, MaxSizeMB: 1, MaxFiles: 3}); err == nil {
		t.Fatal("RotateNow reported success on a log it cannot truncate")
	}
	assertRingIntact(t, path, want)
	if got := mustStatSize(t, path); got != 2*1024*1024 {
		t.Fatalf("live log is %d bytes, want it untouched", got)
	}
}

// TestRotateNowStandsDownForALiveOwner is HIGH-2: the file is perfectly
// truncatable — a probe says "go ahead", because a Go owner grants
// FILE_SHARE_WRITE and takes no POSIX lock — and rotating it anyway is exactly
// the bug. Only an explicit owner can stop it.
func TestRotateNowStandsDownForALiveOwner(t *testing.T) {
	path := newTestLog(t)
	if err := os.WriteFile(path, []byte(strings.Repeat("o", 2*1024*1024)), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := seedRing(t, path, 3)

	opts := Options{Path: path, MaxSizeMB: 1, MaxFiles: 3,
		Owner: func(string) (Owner, bool) { return Owner{PID: 4242, What: "the running unarr daemon"}, true }}

	// The probe must NOT be what saves us: prove it would have said yes.
	if err := probeTruncatable(path); err != nil {
		t.Fatalf("this test is void unless the file is truncatable: %v", err)
	}

	err := RotateNow(opts)
	if !errors.Is(err, ErrOwnedByLiveProcess) {
		t.Fatalf("RotateNow returned %v, want ErrOwnedByLiveProcess", err)
	}
	// The message is what the user reads, so it has to name the file and the pid.
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "4242") {
		t.Fatalf("refusal is not actionable: %v", err)
	}
	assertRingIntact(t, path, want)
	if got := mustStatSize(t, path); got != 2*1024*1024 {
		t.Fatalf("live log is %d bytes, want the owner's file untouched", got)
	}
}

// TestRotateNowProceedsWhenNobodyOwnsTheFile: the probe returning "no owner"
// (a dead PID, a stale state file, a foreground daemon that claimed nothing)
// must not block rotation — a crashed daemon's leftovers cannot be allowed to
// freeze the ring forever.
func TestRotateNowProceedsWhenNobodyOwnsTheFile(t *testing.T) {
	path := newTestLog(t)
	if err := os.WriteFile(path, []byte(strings.Repeat("o", 2*1024*1024)), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	asked := ""
	opts := Options{Path: path, MaxSizeMB: 1, MaxFiles: 3,
		Owner: func(p string) (Owner, bool) { asked = p; return Owner{}, false }}

	if err := RotateNow(opts); err != nil {
		t.Fatalf("RotateNow: %v", err)
	}
	if asked != path {
		t.Fatalf("the owner probe was asked about %q, want %q", asked, path)
	}
	if got := mustStatSize(t, path); got != 0 {
		t.Fatalf("live log is %d bytes, want it trimmed", got)
	}
	if got := len(mustRead(t, RotatedPath(path, 1))); got != 2*1024*1024 {
		t.Fatalf("unarr.log.1 is %d bytes, want the 2 MiB that was live", got)
	}
}

// TestWriterDoesNotRotateAgainAfterAnExternalTrim is the second half of HIGH-2:
// even once the external rotation is refused, an older build (or an operator
// with `> unarr.log`) can still shrink the file underneath the Writer. Its
// in-memory counter would then be a full budget too high, so the NEXT line
// rotated a file that is already empty — a second ring shift, and an empty
// slot 1 covering for the history it displaced.
func TestWriterDoesNotRotateAgainAfterAnExternalTrim(t *testing.T) {
	path := newTestLog(t)
	w, err := NewWriter(Options{Path: path, MaxSizeMB: 1, MaxFiles: 3})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	// Just under the budget, so the very next write would cross it.
	if _, err := w.Write([]byte(strings.Repeat("x", 1<<20-1))); err != nil {
		t.Fatalf("write: %v", err)
	}
	want := seedRing(t, path, 3)

	// Somebody else empties the file — the copy-truncate this design refuses,
	// arriving from an older build or a shell redirect.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("external truncate: %v", err)
	}
	if _, err := w.Write([]byte("one more line\n")); err != nil {
		t.Fatalf("write after the external trim: %v", err)
	}

	assertRingIntact(t, path, want)
	if got := mustRead(t, path); got != "one more line\n" {
		t.Fatalf("live log is %q, want the line that was just written — the Writer "+
			"rotated a file that was already empty", got)
	}
}

// TestRotationCleansUpAStaleStagingFile: a process killed mid-rotation can
// leave a staging file behind. It is not history — nothing reads it — and it
// must never be mistaken for content or block the next rotation.
func TestRotationCleansUpAStaleStagingFile(t *testing.T) {
	path := newTestLog(t)
	if err := os.WriteFile(path, []byte(strings.Repeat("o", 2*1024*1024)), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(path+stagingSuffix, []byte("from a rotation that died"), 0o644); err != nil {
		t.Fatalf("seed staging: %v", err)
	}

	if err := RotateNow(Options{Path: path, MaxSizeMB: 1, MaxFiles: 2}); err != nil {
		t.Fatalf("RotateNow: %v", err)
	}
	if _, err := os.Stat(path + stagingSuffix); !os.IsNotExist(err) {
		t.Fatalf("the staging file survived the rotation (stat err: %v)", err)
	}
	if got := len(mustRead(t, RotatedPath(path, 1))); got != 2*1024*1024 {
		t.Fatalf("unarr.log.1 is %d bytes, want the live log — not the stale staging file", got)
	}
}
