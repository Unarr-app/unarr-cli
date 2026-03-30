package engine

import (
	"context"
	"testing"
	"time"

	"github.com/torrentclaw/unarr/internal/agent"
)

func TestManagerSubmitAndWait(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)

	dl := &mockDownloader{method: MethodTorrent, available: true}
	mgr := NewManager(ManagerConfig{
		MaxConcurrent: 2,
		OutputDir:     t.TempDir(),
	}, reporter, dl)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go reporter.Run(ctx)

	mgr.Submit(ctx, agent.Task{
		ID:              "test-task-1",
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "Test Movie",
		PreferredMethod: "torrent",
	})

	mgr.Wait()

	// Task should have been processed (completed or failed depending on verify)
	// Since mock returns a file that doesn't exist, it may fail at verify
	// This is expected — we're testing the pipeline works
}

func TestManagerHasCapacity(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)

	mgr := NewManager(ManagerConfig{MaxConcurrent: 2}, reporter)

	if !mgr.HasCapacity() {
		t.Error("new manager should have capacity")
	}
}

func TestManagerActiveCount(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)

	mgr := NewManager(ManagerConfig{MaxConcurrent: 3}, reporter)

	if mgr.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0", mgr.ActiveCount())
	}
}

func TestManagerShutdown(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)

	dl := &mockDownloader{method: MethodTorrent, available: true}
	mgr := NewManager(ManagerConfig{
		MaxConcurrent: 1,
		OutputDir:     t.TempDir(),
	}, reporter, dl)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	mgr.Shutdown(ctx)
	// Should not hang
}
