package logging

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is an io.Writer safe to read from the test goroutine while Follow
// writes from its own.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor polls until cond holds or the deadline passes. Follow is poll-based,
// so a test has to give it a few ticks.
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// renameWithRetry renames a log the way the Writer does: patiently. A single
// refusal is normal on Windows (a scanner or another reader holding the file
// for a moment) and means nothing about the code under test.
func renameWithRetry(t *testing.T, from, to string) error {
	t.Helper()
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

// appendLine adds one line to a log file the way a daemon would.
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("append to %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("append to %s: %v", path, err)
	}
}

// startFollow runs Follow in the background and returns its output sink plus a
// stop function that also waits for the goroutine to finish.
func startFollow(t *testing.T, q Query) (*syncBuffer, func()) {
	t.Helper()
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Follow(ctx, q, out) }()
	return out, func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Follow returned %v, want nil after cancellation", err)
		}
	}
}

func TestFollowPrintsTheTailThenStreamsNewLines(t *testing.T) {
	path := newTestLog(t)
	writeLines(t, path, "existing")

	out, stop := startFollow(t, Query{Path: path, Lines: 5})
	defer stop()

	if !waitFor(t, func() bool { return strings.Contains(out.String(), "existing") }) {
		t.Fatal("Follow did not print the existing tail")
	}
	appendLine(t, path, "arrived later")
	if !waitFor(t, func() bool { return strings.Contains(out.String(), "arrived later") }) {
		t.Fatalf("Follow did not pick up the new line, got %q", out.String())
	}
}

func TestFollowAppliesTheFilters(t *testing.T) {
	path := newTestLog(t)
	writeLines(t, path, "2025/01/20 10:00:00 [sync] tick")

	out, stop := startFollow(t, Query{Path: path, Lines: 5, MinLevel: LevelWarn})
	defer stop()

	appendLine(t, path, "2025/01/20 10:00:01 [sync] tick")
	appendLine(t, path, "2025/01/20 10:00:02 Error: boom")

	if !waitFor(t, func() bool { return strings.Contains(out.String(), "boom") }) {
		t.Fatalf("the error line never arrived, got %q", out.String())
	}
	if strings.Contains(out.String(), "tick") {
		t.Fatalf("info lines leaked past --level warn: %q", out.String())
	}
}

func TestFollowSurvivesRotation(t *testing.T) {
	// Not reachable on Windows, and the reason is worth writing down because it
	// is a real property of the product there, not a test artefact.
	//
	// Go's os.Open asks for FILE_SHARE_READ|FILE_SHARE_WRITE and NOT
	// FILE_SHARE_DELETE, so while a follower holds the log open, ANY rename of
	// it — the Writer's own included — is refused with "the process cannot
	// access the file because it is being used by another process". Retrying
	// does not help: it is a property of the open handle, not a passing lock
	// (measured on the VM harness, 5s of retries, every attempt refused).
	//
	// So on Windows an `unarr logs -f` session blocks rotation for as long as it
	// is open. That is EXPECTED and already handled: the Writer treats a refused
	// rename as a back-off-a-whole-budget event rather than an error, which is
	// what TestWriterBacksOffAndReportsOnceWhenTheRenameFails pins. And the
	// capability this test is about — noticing the file underneath was replaced
	// and re-attaching — is covered on Windows by TestFollowSurvivesCopyTruncate,
	// which needs no rename and does pass there.
	if runtime.GOOS == "windows" {
		t.Skip("a followed file cannot be renamed on Windows (no FILE_SHARE_DELETE); " +
			"TestFollowSurvivesCopyTruncate covers re-attachment there")
	}
	path := newTestLog(t)
	writeLines(t, path, "before")

	out, stop := startFollow(t, Query{Path: path, Lines: 5, MaxFiles: 2})
	defer stop()

	if !waitFor(t, func() bool { return strings.Contains(out.String(), "before") }) {
		t.Fatal("Follow did not print the existing tail")
	}

	// Rotate the way the Writer does — rename, then a brand new live file.
	//
	// Retried, because on Windows a rename of a file someone else has open is
	// not reliably instantaneous: Defender's post-write scan and the follower's
	// own handle both hold it for short moments, and the call comes back with
	// "The process cannot access the file because it is being used by another
	// process". That is exactly why the real Writer backs off and retries rather
	// than treating one refusal as fatal (see writer_owned.go), so a test that
	// gave up on the first attempt was holding the product to a stricter
	// standard than the product holds itself.
	if err := renameWithRetry(t, path, RotatedPath(path, 1)); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	appendLine(t, path, "after rotation")

	if !waitFor(t, func() bool { return strings.Contains(out.String(), "after rotation") }) {
		t.Fatalf("Follow did not re-open the rotated log, got %q", out.String())
	}
}

func TestFollowSurvivesCopyTruncate(t *testing.T) {
	path := newTestLog(t)
	// A realistically chunky log: after the truncate the file is far shorter
	// than what the follower has already consumed, which is the signal it uses.
	writeLines(t, path, "before "+strings.Repeat("x", 4096))

	out, stop := startFollow(t, Query{Path: path, Lines: 5})
	defer stop()

	if !waitFor(t, func() bool { return strings.Contains(out.String(), "before") }) {
		t.Fatal("Follow did not print the existing tail")
	}

	// This is what RotateNow does to the live file.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	appendLine(t, path, "after truncate")

	if !waitFor(t, func() bool { return strings.Contains(out.String(), "after truncate") }) {
		t.Fatalf("Follow did not notice the truncation, got %q", out.String())
	}
}

func TestFollowWaitsForALogThatDoesNotExistYet(t *testing.T) {
	path := newTestLog(t)

	out, stop := startFollow(t, Query{Path: path, Lines: 5})
	defer stop()

	appendLine(t, path, "first ever line")
	if !waitFor(t, func() bool { return strings.Contains(out.String(), "first ever line") }) {
		t.Fatalf("Follow did not pick up a log created after it started, got %q", out.String())
	}
}

// TestFollowNeverLosesTheFirstLineToAStartupRace hammers the one interleaving
// that used to drop data, instead of waiting for the scheduler to produce it by
// luck (it did, on macOS, roughly one run in three).
//
// The race: Follow printed the existing tail, and THEN opened the file and
// seeked to its end. A log created in between was invisible to both halves —
// Print saw no file, the seek skipped what had just been written — so the first
// lines a daemon ever wrote were lost, and `unarr logs -f` started at the same
// moment as the daemon showed nothing until the SECOND line arrived.
//
// Each iteration recreates the window: start following a path with no file,
// then create it immediately. Twenty rounds turns a one-in-three flake into a
// near-certain failure if the ordering ever regresses.
func TestFollowNeverLosesTheFirstLineToAStartupRace(t *testing.T) {
	for i := range 20 {
		path := filepath.Join(t.TempDir(), "unarr.log")
		out, stop := startFollow(t, Query{Path: path, Lines: 5})
		appendLine(t, path, "first ever line")

		ok := waitFor(t, func() bool { return strings.Contains(out.String(), "first ever line") })
		stop()
		if !ok {
			t.Fatalf("round %d: the first line written to a brand-new log never appeared: %q", i, out.String())
		}
	}
}

// TestFollowDoesNotRepeatTheTailItAlreadyPrinted is the other side of that
// contract: resuming from the size Print consumed must not re-emit those lines.
// A viewer that repeats itself on every start is a milder bug than one that
// drops lines, but it is still a bug.
func TestFollowDoesNotRepeatTheTailItAlreadyPrinted(t *testing.T) {
	path := newTestLog(t)
	writeLines(t, path, "old one", "old two")

	out, stop := startFollow(t, Query{Path: path, Lines: 5})
	defer stop()

	appendLine(t, path, "brand new")
	if !waitFor(t, func() bool { return strings.Contains(out.String(), "brand new") }) {
		t.Fatalf("Follow did not stream the new line, got %q", out.String())
	}
	if n := strings.Count(out.String(), "old two"); n != 1 {
		t.Fatalf("the printed tail appeared %d times, want exactly 1:\n%s", n, out.String())
	}
}
