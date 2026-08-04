package engine

import (
	"testing"
	"time"
)

// waitFor polls until cond holds or the deadline passes. The manager does its
// terminal bookkeeping on the download goroutine, so assertions right after a
// Pause/Cancel would race it.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A pause must stay resumable. The pause cancels the task context, so the
// download goroutine unwinds through fail() → recordFinished, which drops the
// resume entry for a genuine terminal — and would silently turn "pause" into
// "start over from zero after a restart".
func TestManager_PauseKeepsResumeEntry(t *testing.T) {
	store := newFakePersister()
	mgr, ctx, cancel := newResumeManager(t, store)
	defer cancel()

	task := dlTask("pause-keeps")
	mgr.Submit(ctx, task)
	waitFor(t, "task to be persisted", func() bool { return store.has(task.ID) })

	if !mgr.PauseTask(task.ID) {
		t.Fatal("PauseTask reported the task was not running")
	}

	// Give the goroutine time to unwind and do its bookkeeping.
	time.Sleep(200 * time.Millisecond)
	if !store.has(task.ID) {
		t.Fatal("pausing dropped the resume entry — the download would restart from zero")
	}
}

// The mutation that proves the guard guards: without the paused-set check, the
// unwinding goroutine's recordFinished removes the entry. Assert the mechanism
// directly, so a refactor that drops the set fails here rather than silently
// costing users their partial downloads.
func TestManager_PausedSetDrivesResumeRetention(t *testing.T) {
	store := newFakePersister()
	mgr, ctx, cancel := newResumeManager(t, store)
	defer cancel()

	task := dlTask("pause-mechanism")
	mgr.Submit(ctx, task)
	waitFor(t, "task to be persisted", func() bool { return store.has(task.ID) })

	mgr.markPaused(task.ID)
	if !mgr.isPaused(task.ID) {
		t.Fatal("markPaused did not register the task")
	}
	// recordFinished is what the unwinding goroutine calls.
	mgr.recordFinished(mgr.GetTask(task.ID).ToStatusUpdate())
	if !store.has(task.ID) {
		t.Fatal("a paused task lost its resume entry on recordFinished")
	}

	// Same call, no pause marker → the entry must go (genuine terminal).
	mgr.clearPaused(task.ID)
	mgr.recordFinished(mgr.GetTask(task.ID).ToStatusUpdate())
	if store.has(task.ID) {
		t.Fatal("a terminal task kept its resume entry — it would be resurrected on restart")
	}
}

// A cancel is final: the resume entry must go immediately, not "eventually,
// once the goroutine unwinds", because a shutdown racing the cancel keeps
// entries on purpose and would preserve this one.
func TestManager_CancelDropsResumeEntryImmediately(t *testing.T) {
	store := newFakePersister()
	mgr, ctx, cancel := newResumeManager(t, store)
	defer cancel()

	task := dlTask("cancel-drops")
	mgr.Submit(ctx, task)
	waitFor(t, "task to be persisted", func() bool { return store.has(task.ID) })

	if !mgr.CancelTask(task.ID) {
		t.Fatal("CancelTask reported the task was not running")
	}
	if store.has(task.ID) {
		t.Fatal("cancel left the resume entry behind — the download comes back on the next start")
	}
}

// Cancelling during shutdown must still drop the entry. shuttingDown makes
// recordFinished KEEP entries (so an interrupted download resumes), which is
// exactly the window where a user-cancelled task could survive as a zombie.
func TestManager_CancelDuringShutdownStillDropsResume(t *testing.T) {
	store := newFakePersister()
	mgr, ctx, cancel := newResumeManager(t, store)
	defer cancel()

	task := dlTask("cancel-shutdown")
	mgr.Submit(ctx, task)
	waitFor(t, "task to be persisted", func() bool { return store.has(task.ID) })

	mgr.shuttingDown.Store(true)
	mgr.CancelTask(task.ID)

	if store.has(task.ID) {
		t.Fatal("a cancel during shutdown kept the resume entry")
	}
}

// Re-submitting a paused task is a fresh run: the pause marker must clear, or
// the NEXT genuine terminal would keep a stale resume entry forever.
func TestManager_SubmitClearsPauseMarker(t *testing.T) {
	store := newFakePersister()
	mgr, ctx, cancel := newResumeManager(t, store)
	defer cancel()

	task := dlTask("resubmit-clears")
	mgr.Submit(ctx, task)
	waitFor(t, "task to be persisted", func() bool { return store.has(task.ID) })
	mgr.PauseTask(task.ID)
	waitFor(t, "task to leave the active set", func() bool { return mgr.GetTask(task.ID) == nil })

	mgr.Submit(ctx, task)
	if mgr.isPaused(task.ID) {
		t.Fatal("re-submitting left the task marked as paused")
	}
}

// DropResume is the zombie button: it must clear the store entry for a task
// that is not running at all (nothing else can, since Cancel needs a live task).
func TestManager_DropResumeWorksWithoutRunningTask(t *testing.T) {
	store := newFakePersister()
	mgr, _, cancel := newResumeManager(t, store)
	defer cancel()

	store.Add(dlTask("orphan"))
	id := dlTask("orphan").ID
	if !store.has(id) {
		t.Fatal("fixture: entry not stored")
	}

	mgr.DropResume(id)
	if store.has(id) {
		t.Fatal("DropResume left the entry — a restart would resurrect the download")
	}
}

// PausedTaskIDs feeds the "paused" label in `unarr downloads` and the tray.
func TestManager_PausedTaskIDs(t *testing.T) {
	mgr, _, cancel := newResumeManager(t, newFakePersister())
	defer cancel()

	mgr.markPaused("task-uuid-a")
	mgr.markPaused("task-uuid-b")
	mgr.clearPaused("task-uuid-a")

	ids := mgr.PausedTaskIDs()
	if len(ids) != 1 || ids[0] != "task-uuid-b" {
		t.Fatalf("PausedTaskIDs = %v, want [task-uuid-b]", ids)
	}
}
