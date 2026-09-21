package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// fakePersister is an in-memory taskPersister for asserting manager↔store calls
// without touching disk.
type fakePersister struct {
	mu    sync.Mutex
	tasks map[string]bool
}

func newFakePersister() *fakePersister      { return &fakePersister{tasks: map[string]bool{}} }
func (f *fakePersister) Add(t agent.Task)   { f.mu.Lock(); f.tasks[t.ID] = true; f.mu.Unlock() }
func (f *fakePersister) Remove(id string)   { f.mu.Lock(); delete(f.tasks, id); f.mu.Unlock() }
func (f *fakePersister) has(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.tasks[id] }

func newResumeManager(t *testing.T, p taskPersister) (*Manager, context.Context, context.CancelFunc) {
	t.Helper()
	reporter := NewProgressReporter(agent.NewClient("http://localhost", "test", "test"), time.Hour)
	mgr := NewManager(
		ManagerConfig{MaxConcurrent: 2, OutputDir: t.TempDir()},
		reporter,
		&slowMockDownloader{method: MethodTorrent},
	)
	mgr.SetTaskStore(p)
	ctx, cancel := context.WithCancel(context.Background())
	go reporter.Run(ctx)
	return mgr, ctx, cancel
}

// dlTask builds a download task. IDs mirror production (UUID-length) because the
// web always sends UUIDs. Short ids are safe now — the log prefixes go through
// Task.ShortID(), which clamps instead of slicing — but keep them realistic.
func dlTask(id string) agent.Task {
	return agent.Task{
		ID:              "task-uuid-" + id, // ≥ 8 chars like a real dispatch id
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "Resume " + id,
		PreferredMethod: "torrent",
		Mode:            "download",
	}
}

func TestManager_SubmitDedupes(t *testing.T) {
	mgr, ctx, cancel := newResumeManager(t, newFakePersister())
	defer cancel()

	task := dlTask("dup-1")
	mgr.Submit(ctx, task)
	mgr.Submit(ctx, task) // duplicate id — must not launch a second download

	if n := mgr.ActiveCount(); n != 1 {
		t.Errorf("ActiveCount = %d after duplicate submit, want 1", n)
	}
	cancel()
	mgr.Wait()
}

func TestManager_PersistsDownloadAndRemovesOnTerminal(t *testing.T) {
	p := newFakePersister()
	mgr, ctx, cancel := newResumeManager(t, p)
	defer cancel()

	task := dlTask("t1")
	mgr.Submit(ctx, task)
	if !p.has(task.ID) {
		t.Fatal("download not persisted to the resume store on submit")
	}

	// A genuine terminal (user cancel, not shutdown) must remove it.
	mgr.CancelTask(task.ID)
	mgr.Wait()
	if p.has(task.ID) {
		t.Error("task still in resume store after a genuine terminal — should be removed")
	}
}

func TestManager_KeepsStoreEntryOnShutdown(t *testing.T) {
	p := newFakePersister()
	mgr, ctx, cancel := newResumeManager(t, p)
	defer cancel()

	task := dlTask("s1")
	mgr.Submit(ctx, task)
	if !p.has(task.ID) {
		t.Fatal("download not persisted on submit")
	}

	// Shutdown interrupts the in-flight download — the entry must SURVIVE so the
	// daemon re-submits and resumes it next start.
	// Shutdown cancels the task contexts itself then waits, so once it returns
	// the interrupted task's recordFinished has run (and must have skipped the
	// removal because shuttingDown is set) — no sleep/poll needed.
	shutCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	mgr.Shutdown(shutCtx)

	if !p.has(task.ID) {
		t.Error("task removed from resume store on shutdown — it would not resume")
	}
}

// TestManager_ShutdownDoesNotFailInFlightDownloads: stopping the daemon is not a
// download failure. The interrupted task must not turn FAILED nor be queued for
// the server as failed — before, every restart left the in-flight downloads on
// the dashboard as "failed: context canceled" while the resume store quietly
// re-ran them on the next start (seen in a 177-episode debrid run).
func TestManager_ShutdownDoesNotFailInFlightDownloads(t *testing.T) {
	p := newFakePersister()
	mgr, ctx, cancel := newResumeManager(t, p)
	defer cancel()

	task := dlTask("s2")
	mgr.Submit(ctx, task)
	running := mgr.GetTask(task.ID)
	if running == nil {
		t.Fatal("task not active after submit")
	}

	shutCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	mgr.Shutdown(shutCtx)

	if s := running.GetStatus(); s == StatusFailed {
		t.Errorf("in-flight download marked %s by a shutdown — it is only interrupted", s)
	}
	for _, st := range mgr.TaskStates() {
		if st.TaskID == task.ID && st.Status == string(StatusFailed) {
			t.Errorf("shutdown queued the interrupted task for the server as failed: %+v", st)
		}
	}
	if !p.has(task.ID) {
		t.Error("task removed from resume store on shutdown — it would not resume")
	}
}

func TestManager_DoesNotPersistStreamTasks(t *testing.T) {
	p := newFakePersister()
	mgr, ctx, cancel := newResumeManager(t, p)
	defer cancel()

	task := dlTask("stream-1")
	task.Mode = "stream"
	mgr.Submit(ctx, task)
	if p.has(task.ID) {
		t.Error("stream task persisted to resume store — only downloads should be")
	}
	cancel()
	mgr.Wait()
}
