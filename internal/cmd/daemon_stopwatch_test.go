package cmd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// redirectDataDir points config.DataDir() (and so the stop-intent marker) at a
// temp dir. The env var differs per platform, so set both.
func redirectDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir) // linux
	t.Setenv("LOCALAPPDATA", dir)  // windows
	t.Setenv("HOME", dir)          // darwin fallback
	return filepath.Join(dir, "unarr")
}

func shortenPoll(t *testing.T) {
	t.Helper()
	orig := stopIntentPoll
	stopIntentPoll = 20 * time.Millisecond
	t.Cleanup(func() { stopIntentPoll = orig })
}

// TestWatchStopIntentFiresOnTheMarker is the mechanism that makes a stop work
// when the state file cannot be trusted: the daemon is TOLD to stop rather than
// hunted down by PID. On Windows that is the only reliable channel — there are
// no signals, and ending the scheduled task leaves the daemon orphaned but very
// much alive (measured).
func TestWatchStopIntentFiresOnTheMarker(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS derives the data dir from HOME in a way this shim does not fake")
	}
	redirectDataDir(t)
	shortenPoll(t)
	agent.ClearStopIntent()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	go watchStopIntent(ctx, sigCh)

	// Nothing asked for a stop yet: the watcher must stay quiet.
	select {
	case s := <-sigCh:
		t.Fatalf("watcher signalled %v with no stop requested", s)
	case <-time.After(200 * time.Millisecond):
	}

	agent.WriteStopIntent()
	select {
	case s := <-sigCh:
		if s != syscall.SIGTERM {
			t.Errorf("watcher sent %v, want SIGTERM (the existing shutdown path)", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher never noticed the stop marker — `unarr stop` would not stop this daemon")
	}
}

// TestWatchStopIntentStopsWithTheDaemon: the watcher must not outlive the
// daemon's context, or a test (or a restarted daemon in-process) would leak it.
func TestWatchStopIntentStopsWithTheDaemon(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS derives the data dir from HOME in a way this shim does not fake")
	}
	redirectDataDir(t)
	shortenPoll(t)
	agent.ClearStopIntent()

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() { watchStopIntent(ctx, sigCh); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher ignored context cancellation")
	}
}

// TestWatchStopIntentNeverBlocks: a shutdown already under way leaves nobody
// reading sigCh. The watcher must not wedge on the send.
func TestWatchStopIntentNeverBlocks(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS derives the data dir from HOME in a way this shim does not fake")
	}
	redirectDataDir(t)
	shortenPoll(t)
	agent.WriteStopIntent()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	full := make(chan os.Signal) // unbuffered, nobody receiving
	done := make(chan struct{})
	go func() { watchStopIntent(ctx, full); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher blocked trying to signal a shutdown already in progress")
	}
}

// TestDaemonStartsTheStopWatcher pins the wiring: an unstarted watcher is a
// daemon that cannot be stopped on Windows.
func TestDaemonStartsTheStopWatcher(t *testing.T) {
	src, err := os.ReadFile("daemon.go")
	if err != nil {
		t.Fatalf("read daemon.go: %v", err)
	}
	if !strings.Contains(string(src), "go watchStopIntent(ctx, sigCh)") {
		t.Error("the daemon no longer watches the stop marker — `unarr stop` cannot reach a daemon the state file does not name")
	}
}
