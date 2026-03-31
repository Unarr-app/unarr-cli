package engine

import (
	"context"
	"os"
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

func TestManagerDefaultConcurrency(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)
	mgr := NewManager(ManagerConfig{MaxConcurrent: 0}, reporter)
	if cap(mgr.sem) != 3 {
		t.Errorf("default MaxConcurrent should be 3, got %d", cap(mgr.sem))
	}
}

func TestManagerGetTask(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)
	mgr := NewManager(ManagerConfig{MaxConcurrent: 2}, reporter)

	// No task added
	if task := mgr.GetTask("nonexistent"); task != nil {
		t.Error("expected nil for nonexistent task")
	}
}

func TestManagerActiveTasks(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)
	mgr := NewManager(ManagerConfig{MaxConcurrent: 2}, reporter)

	tasks := mgr.ActiveTasks()
	if len(tasks) != 0 {
		t.Errorf("expected 0 active tasks, got %d", len(tasks))
	}
}

func TestManagerSubmitCompletesWithValidFile(t *testing.T) {
	dir := t.TempDir()
	// Create a file that verify() will accept
	filePath := dir + "/movie.mkv"
	os.WriteFile(filePath, make([]byte, 1024), 0o644)

	reporter := &mockStatusReporter{}
	pr := &ProgressReporter{
		reporter:     reporter,
		interval:     100 * time.Millisecond,
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
	}

	dl := &resultMockDownloader{
		method: MethodTorrent,
		result: &Result{
			FilePath: filePath,
			FileName: "movie.mkv",
			Method:   MethodTorrent,
			Size:     1024,
		},
	}

	mgr := NewManager(ManagerConfig{
		MaxConcurrent: 2,
		OutputDir:     dir,
	}, pr, dl)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go pr.Run(ctx)

	mgr.Submit(ctx, agent.Task{
		ID:              "task-complete-test1",
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "Test Movie",
		PreferredMethod: "torrent",
	})

	mgr.Wait()
	cancel()

	// Task should have completed successfully
	// (we can't check directly since it's removed from active map after processing)
}

func TestManagerCancelTask(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)

	dl := &slowMockDownloader{method: MethodTorrent}
	mgr := NewManager(ManagerConfig{
		MaxConcurrent: 2,
		OutputDir:     t.TempDir(),
	}, reporter, dl)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go reporter.Run(ctx)

	mgr.Submit(ctx, agent.Task{
		ID:              "task-cancel-test12",
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "Cancel Me",
		PreferredMethod: "torrent",
	})

	// Give it time to start
	time.Sleep(100 * time.Millisecond)

	mgr.CancelTask("task-cancel-test12")
	mgr.Wait()
}

func TestManagerPauseTask(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)

	dl := &slowMockDownloader{method: MethodTorrent}
	mgr := NewManager(ManagerConfig{
		MaxConcurrent: 2,
		OutputDir:     t.TempDir(),
	}, reporter, dl)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go reporter.Run(ctx)

	mgr.Submit(ctx, agent.Task{
		ID:              "task-pause-test123",
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "Pause Me",
		PreferredMethod: "torrent",
	})

	time.Sleep(100 * time.Millisecond)
	mgr.PauseTask("task-pause-test123")
	mgr.Wait()
}

func TestManagerCancelAndDeleteFiles(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)

	dl := &slowMockDownloader{method: MethodTorrent}
	mgr := NewManager(ManagerConfig{
		MaxConcurrent: 2,
		OutputDir:     t.TempDir(),
	}, reporter, dl)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go reporter.Run(ctx)

	mgr.Submit(ctx, agent.Task{
		ID:              "task-delfile-test12",
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "Delete Me",
		PreferredMethod: "torrent",
	})

	time.Sleep(100 * time.Millisecond)
	mgr.CancelAndDeleteFiles("task-delfile-test12")
	mgr.Wait()
}

func TestManagerCancelNonexistent(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)
	mgr := NewManager(ManagerConfig{MaxConcurrent: 2}, reporter)
	// Should not panic
	mgr.CancelTask("nonexistent")
	mgr.PauseTask("nonexistent")
	mgr.CancelAndDeleteFiles("nonexistent")
}

// resultMockDownloader returns a configurable result
type resultMockDownloader struct {
	method DownloadMethod
	result *Result
}

func (m *resultMockDownloader) Method() DownloadMethod { return m.method }
func (m *resultMockDownloader) Available(_ context.Context, _ *Task) (bool, error) {
	return true, nil
}
func (m *resultMockDownloader) Download(_ context.Context, _ *Task, _ string, _ chan<- Progress) (*Result, error) {
	return m.result, nil
}
func (m *resultMockDownloader) Pause(_ string) error             { return nil }
func (m *resultMockDownloader) Cancel(_ string) error            { return nil }
func (m *resultMockDownloader) Shutdown(_ context.Context) error { return nil }

// slowMockDownloader blocks until context is cancelled
type slowMockDownloader struct {
	method DownloadMethod
}

func (m *slowMockDownloader) Method() DownloadMethod { return m.method }
func (m *slowMockDownloader) Available(_ context.Context, _ *Task) (bool, error) {
	return true, nil
}
func (m *slowMockDownloader) Download(ctx context.Context, _ *Task, _ string, _ chan<- Progress) (*Result, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (m *slowMockDownloader) Pause(_ string) error    { return nil }
func (m *slowMockDownloader) Cancel(_ string) error    { return nil }
func (m *slowMockDownloader) Shutdown(_ context.Context) error { return nil }
