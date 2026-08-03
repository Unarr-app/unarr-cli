package logging

import (
	"bytes"
	"context"
	"os"
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
	path := newTestLog(t)
	writeLines(t, path, "before")

	out, stop := startFollow(t, Query{Path: path, Lines: 5, MaxFiles: 2})
	defer stop()

	if !waitFor(t, func() bool { return strings.Contains(out.String(), "before") }) {
		t.Fatal("Follow did not print the existing tail")
	}

	// Rotate the way the Writer does — rename, then a brand new live file.
	if err := os.Rename(path, RotatedPath(path, 1)); err != nil {
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
