package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
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
func (m *slowMockDownloader) Pause(_ string) error             { return nil }
func (m *slowMockDownloader) Cancel(_ string) error            { return nil }
func (m *slowMockDownloader) Shutdown(_ context.Context) error { return nil }

// extractPackedRelease must NOT delete the archive parts while the torrent is
// seeding: anacrolix keeps serving the EXACT files it downloaded, so removing
// the .rNN volumes breaks the swarm's requests and the user's ratio obligation
// on a private tracker.
func TestExtractPackedRelease_KeepsPartsWhileSeeding(t *testing.T) {
	dir := t.TempDir()
	// A release that is already unpacked: an extraction is not needed for the
	// cleanup decision, which is what this test pins down.
	for _, name := range []string{"show.part01.rar", "show.part02.rar", "show.mkv"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	m := &Manager{cfg: ManagerConfig{SeedEnabled: true}}
	task := &Task{ID: "seed-test"}
	m.extractPackedRelease(task, &Result{FilePath: dir, Method: MethodTorrent})

	for _, name := range []string{"show.part01.rar", "show.part02.rar"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("archive part %s deleted while seeding: %v", name, err)
		}
	}
}

// COUNTERFACTUAL for the test above: with seeding OFF the parts DO get cleaned
// up. Without this, TestExtractPackedRelease_KeepsPartsWhileSeeding would still
// pass if the cleanup were dead code that never ran at all.
//
// Uses a REAL archive: cleanup only runs after a SUCCESSFUL extraction, so
// stub files named .rar make the extractor fail and the parts survive for the
// wrong reason (which is exactly how this test failed when first written).
func TestExtractPackedRelease_CleansPartsWhenNotSeeding(t *testing.T) {
	sz, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z not installed")
	}

	work := t.TempDir()
	payload := filepath.Join(work, "show.mkv")
	// Incompressible, so the archive really splits into volumes.
	data := make([]byte, 2_000_000)
	seed := uint32(0x12345678)
	for i := range data {
		seed ^= seed << 13
		seed ^= seed >> 17
		seed ^= seed << 5
		data[i] = byte(seed)
	}
	if err := os.WriteFile(payload, data, 0o644); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(work, "release")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sz, "a", "-tzip", "-v1m", filepath.Join(dir, "show.zip"), payload)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not build fixture: %v\n%s", err, b)
	}
	os.Remove(payload)

	before, _ := os.ReadDir(dir)
	if len(before) < 2 {
		t.Fatalf("fixture did not split: %d file(s)", len(before))
	}

	m := &Manager{cfg: ManagerConfig{SeedEnabled: false}}
	m.extractPackedRelease(&Task{ID: "noseed-test"}, &Result{FilePath: dir, Method: MethodTorrent})

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range left {
		if e.Name() != "show.mkv" {
			t.Errorf("archive part %s survived cleanup with seeding off", e.Name())
		}
	}
	// The extracted payload must never be touched.
	if _, err := os.Stat(filepath.Join(dir, "show.mkv")); err != nil {
		t.Errorf("cleanup removed the extracted video: %v", err)
	}
}

// A single-file result cannot be a packed release; it must be left alone.
func TestExtractPackedRelease_IgnoresSingleFileResult(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{cfg: ManagerConfig{SeedEnabled: false}}
	m.extractPackedRelease(&Task{ID: "single"}, &Result{FilePath: file, Method: MethodTorrent})

	if _, err := os.Stat(file); err != nil {
		t.Errorf("single-file result was touched: %v", err)
	}
}
