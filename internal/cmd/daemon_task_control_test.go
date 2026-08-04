package cmd

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/control"
	"github.com/Unarr-app/unarr-cli/internal/engine"
)

// stallingDownloader keeps a download "in progress" until its context is
// cancelled, so pause/cancel act on something that is genuinely running.
type stallingDownloader struct{ method engine.DownloadMethod }

func (d *stallingDownloader) Method() engine.DownloadMethod { return d.method }
func (d *stallingDownloader) Available(context.Context, *engine.Task) (bool, error) {
	return true, nil
}
func (d *stallingDownloader) Download(ctx context.Context, _ *engine.Task, _ string, _ chan<- engine.Progress) (*engine.Result, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (d *stallingDownloader) Pause(string) error  { return nil }
func (d *stallingDownloader) Cancel(string) error { return nil }
func (d *stallingDownloader) Shutdown(context.Context) error {
	return nil
}

// newTestController wires a real Manager + a real (temp-dir backed) resume
// store, because the interesting behaviour is exactly how those two interact.
func newTestController(t *testing.T) (*taskController, context.CancelFunc) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	reporter := engine.NewProgressReporter(nil, time.Hour)
	mgr := engine.NewManager(
		engine.ManagerConfig{MaxConcurrent: 3, OutputDir: t.TempDir()},
		reporter,
		&stallingDownloader{method: engine.MethodTorrent},
	)
	store := agent.NewActiveTaskStore()
	mgr.SetTaskStore(store)

	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	stopped := map[string]bool{}

	ctrl := &taskController{
		manager:     mgr,
		store:       store,
		client:      nil, // no server: report() is a no-op, actions still apply
		agentID:     "test-agent",
		submit:      func(task agent.Task) { mgr.Submit(ctx, task) },
		stopStream:  func(id string) { mu.Lock(); stopped[id] = true; mu.Unlock() },
		triggerSync: func() {},
	}
	return ctrl, cancel
}

func ctlTask(id string) agent.Task {
	return agent.Task{
		ID:              "task-uuid-" + id,
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "Title " + id,
		PreferredMethod: "torrent",
		Mode:            "download",
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
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

// THE regression: a task the server forgot (deleted row) is not in any control
// list the web can send, so only the agent can end it. Cancel must work on a
// task that is merely persisted, and must drop the resume entry — otherwise the
// next daemon start brings the download straight back.
func TestTaskController_CancelZombieNotRunning(t *testing.T) {
	ctrl, cancel := newTestController(t)
	defer cancel()

	zombie := ctlTask("zombie")
	ctrl.store.Add(zombie)

	res := ctrl.Cancel(zombie.ID, false)
	if !res.Applied {
		t.Fatalf("cancelling a persisted-but-not-running task did nothing: %+v", res)
	}
	if _, ok := ctrl.store.Get(zombie.ID); ok {
		t.Fatal("the resume entry survived the cancel — the download would come back")
	}
}

func TestTaskController_CancelUnknownTaskReportsNotFound(t *testing.T) {
	ctrl, cancel := newTestController(t)
	defer cancel()

	res := ctrl.Cancel("task-uuid-nothing", false)
	if res.Applied {
		t.Fatal("cancelling a task nobody has claimed to have succeeded")
	}
	if res.Message != "not found" {
		t.Fatalf("message = %q, want \"not found\"", res.Message)
	}
}

func TestTaskController_PauseThenResumeUsesStoredPayload(t *testing.T) {
	ctrl, cancel := newTestController(t)
	defer cancel()

	task := ctlTask("pause-resume")
	ctrl.submit(task)
	waitUntil(t, "task to start", func() bool { return ctrl.manager.GetTask(task.ID) != nil })

	if res := ctrl.Pause(task.ID); !res.Applied {
		t.Fatalf("pause did not apply: %+v", res)
	}
	waitUntil(t, "task to stop running", func() bool { return ctrl.manager.GetTask(task.ID) == nil })
	if _, ok := ctrl.store.Get(task.ID); !ok {
		t.Fatal("pause dropped the payload — resume would have nothing to run")
	}

	if res := ctrl.Resume(task.ID); !res.Applied {
		t.Fatalf("resume did not apply: %+v", res)
	}
	waitUntil(t, "task to run again", func() bool { return ctrl.manager.GetTask(task.ID) != nil })
}

func TestTaskController_ResumeRunningTaskIsANoOp(t *testing.T) {
	ctrl, cancel := newTestController(t)
	defer cancel()

	task := ctlTask("already-running")
	ctrl.submit(task)
	waitUntil(t, "task to start", func() bool { return ctrl.manager.GetTask(task.ID) != nil })

	res := ctrl.Resume(task.ID)
	if res.Applied {
		t.Fatal("resuming a running download reported an action")
	}
	if res.Message != "already running" {
		t.Fatalf("message = %q", res.Message)
	}
}

// With no stored payload the agent cannot rebuild the task — only the server
// can. Resume must say so instead of silently doing nothing.
func TestTaskController_ResumeWithoutPayloadAsksTheServer(t *testing.T) {
	ctrl, cancel := newTestController(t)
	defer cancel()

	var synced bool
	ctrl.triggerSync = func() { synced = true }

	res := ctrl.Resume("task-uuid-unknown")
	if !res.Applied || res.Message != "asked the server to re-queue it" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !synced {
		t.Fatal("resume did not trigger a sync, so the server would never hear about it")
	}
}

// Purge is the "stop resurrecting what I killed" button. It must clear stopped
// leftovers and must NOT touch a live download.
func TestTaskController_PurgeSparesRunningTasks(t *testing.T) {
	ctrl, cancel := newTestController(t)
	defer cancel()

	live := ctlTask("live")
	ctrl.submit(live)
	waitUntil(t, "task to start", func() bool { return ctrl.manager.GetTask(live.ID) != nil })

	leftover := ctlTask("leftover")
	ctrl.store.Add(leftover)

	results := ctrl.Purge()
	if len(results) != 1 || results[0].TaskID != leftover.ID {
		t.Fatalf("purge results = %+v, want only the leftover", results)
	}
	if _, ok := ctrl.store.Get(live.ID); !ok {
		t.Fatal("purge dropped the resume entry of a RUNNING download")
	}
	if ctrl.manager.GetTask(live.ID) == nil {
		t.Fatal("purge stopped a running download")
	}
}

func TestTaskController_ReapUnknownStopsAndForgets(t *testing.T) {
	ctrl, cancel := newTestController(t)
	defer cancel()

	task := ctlTask("reaped")
	ctrl.submit(task)
	waitUntil(t, "task to start", func() bool { return ctrl.manager.GetTask(task.ID) != nil })

	ctrl.ReapUnknown(task.ID)

	waitUntil(t, "task to stop", func() bool { return ctrl.manager.GetTask(task.ID) == nil })
	if _, ok := ctrl.store.Get(task.ID); ok {
		t.Fatal("a reaped task kept its resume entry — the zombie would return on restart")
	}
}

// List must show BOTH what is running and what is only persisted: the leftovers
// are precisely what the user is hunting when a download will not die.
func TestTaskController_ListIncludesPersistedAndRunning(t *testing.T) {
	ctrl, cancel := newTestController(t)
	defer cancel()

	live := ctlTask("shown-live")
	ctrl.submit(live)
	waitUntil(t, "task to start", func() bool { return ctrl.manager.GetTask(live.ID) != nil })
	ctrl.store.Add(ctlTask("shown-stopped"))

	byID := map[string]control.TaskInfo{}
	for _, info := range ctrl.List() {
		byID[info.ID] = info
	}
	if len(byID) != 2 {
		t.Fatalf("List returned %d rows, want 2: %+v", len(byID), byID)
	}
	if !byID[live.ID].Running {
		t.Error("the running task is not marked Running")
	}
	if !byID[live.ID].Persisted {
		t.Error("the running task should also report Persisted (it is in the resume store)")
	}
	stopped := byID[ctlTask("shown-stopped").ID]
	if stopped.Running {
		t.Error("a store-only entry is marked Running")
	}
	if stopped.State != "stopped" {
		t.Errorf("store-only state = %q, want \"stopped\"", stopped.State)
	}
}

// A paused task must read as "paused", not as an anonymous leftover: the user
// decides between Resume and Stop based on that word.
func TestTaskController_ListLabelsPausedTasks(t *testing.T) {
	ctrl, cancel := newTestController(t)
	defer cancel()

	task := ctlTask("labelled")
	ctrl.submit(task)
	waitUntil(t, "task to start", func() bool { return ctrl.manager.GetTask(task.ID) != nil })
	ctrl.Pause(task.ID)
	waitUntil(t, "task to stop running", func() bool { return ctrl.manager.GetTask(task.ID) == nil })

	for _, info := range ctrl.List() {
		if info.ID == task.ID {
			if info.State != "paused" {
				t.Fatalf("state = %q, want \"paused\"", info.State)
			}
			return
		}
	}
	t.Fatal("the paused task disappeared from the list")
}

// The loop this guard exists for, reproduced: pause lives on the task row, and
// the server keeps re-sending it (sync controls AND the SSE downlink) until the
// row stops saying "paused". A local resume therefore races its own report — and
// without the grace window the in-flight pause stops the download again, every
// single time (observed live: `unarr downloads resume` followed by six
// `downlink: control pause` in the same second, task never leaving paused).
func TestTaskController_StalePauseDoesNotUndoLocalResume(t *testing.T) {
	ctrl, cancel := newTestController(t)
	defer cancel()

	task := ctlTask("resume-race")
	ctrl.submit(task)
	waitUntil(t, "task to start", func() bool { return ctrl.manager.GetTask(task.ID) != nil })
	ctrl.Pause(task.ID)
	waitUntil(t, "task to stop", func() bool { return ctrl.manager.GetTask(task.ID) == nil })

	if res := ctrl.Resume(task.ID); !res.Applied {
		t.Fatalf("resume did not apply: %+v", res)
	}
	waitUntil(t, "task to run again", func() bool { return ctrl.manager.GetTask(task.ID) != nil })

	// The server, still holding a "paused" row, re-sends pause.
	ctrl.ApplyWebAction(control.ActionPause, task.ID, false)

	// It must be ignored: the download keeps running.
	time.Sleep(150 * time.Millisecond)
	if ctrl.manager.GetTask(task.ID) == nil {
		t.Fatal("a stale pause stopped a download that was just resumed here")
	}
}

// The grace window is narrow on purpose: a pause the user asks for AFTER it has
// expired must still work.
func TestTaskController_PauseWorksOnceGraceExpires(t *testing.T) {
	ctrl, cancel := newTestController(t)
	defer cancel()

	task := ctlTask("grace-expiry")
	ctrl.submit(task)
	waitUntil(t, "task to start", func() bool { return ctrl.manager.GetTask(task.ID) != nil })

	ctrl.markResumed(task.ID)
	if !ctrl.recentlyResumed(task.ID) {
		t.Fatal("markResumed did not open the grace window")
	}

	// Age it past the window.
	ctrl.resumedMu.Lock()
	ctrl.resumedAt[task.ID] = time.Now().Add(-resumeGrace - time.Second)
	ctrl.resumedMu.Unlock()

	if ctrl.recentlyResumed(task.ID) {
		t.Fatal("the grace window did not expire")
	}
	ctrl.ApplyWebAction(control.ActionPause, task.ID, false)
	waitUntil(t, "the task to pause", func() bool { return ctrl.manager.GetTask(task.ID) == nil })
}

// A cancel outranks the grace window: asking for a download to END beats having
// asked for it to continue a moment earlier.
func TestTaskController_CancelBeatsResumeGrace(t *testing.T) {
	ctrl, cancel := newTestController(t)
	defer cancel()

	task := ctlTask("cancel-wins")
	ctrl.submit(task)
	waitUntil(t, "task to start", func() bool { return ctrl.manager.GetTask(task.ID) != nil })
	ctrl.markResumed(task.ID)

	ctrl.ApplyWebAction(control.ActionCancel, task.ID, false)

	waitUntil(t, "the task to stop", func() bool { return ctrl.manager.GetTask(task.ID) == nil })
	if _, ok := ctrl.store.Get(task.ID); ok {
		t.Fatal("cancel during the grace window left the resume entry behind")
	}
}

// Retry restarts a task whose row usually still says "paused" or "cancelled",
// so it races the same re-sent control that used to undo Resume. It must open
// the grace window too, or `unarr downloads retry` on a paused download bounces
// straight back to paused.
func TestTaskController_RetryOpensResumeGrace(t *testing.T) {
	ctrl, cancel := newTestController(t)
	defer cancel()

	task := ctlTask("retry-race")
	ctrl.submit(task)
	waitUntil(t, "task to start", func() bool { return ctrl.manager.GetTask(task.ID) != nil })
	ctrl.Pause(task.ID)
	waitUntil(t, "task to stop", func() bool { return ctrl.manager.GetTask(task.ID) == nil })

	if res := ctrl.Retry(task.ID); !res.Applied {
		t.Fatalf("retry did not apply: %+v", res)
	}
	waitUntil(t, "task to run again", func() bool { return ctrl.manager.GetTask(task.ID) != nil })

	// The server, still holding the pre-retry row, re-sends pause.
	ctrl.ApplyWebAction(control.ActionPause, task.ID, false)

	time.Sleep(150 * time.Millisecond)
	if ctrl.manager.GetTask(task.ID) == nil {
		t.Fatal("a stale pause stopped a download that was just retried here")
	}
}

// A pause asked for locally closes the grace window: the task is deliberately
// stopped, so a pause arriving afterwards is a real instruction, not an echo —
// and the window must not outlive the download that opened it.
func TestTaskController_LocalPauseClosesResumeGrace(t *testing.T) {
	ctrl, cancel := newTestController(t)
	defer cancel()

	task := ctlTask("pause-clears-grace")
	ctrl.submit(task)
	waitUntil(t, "task to start", func() bool { return ctrl.manager.GetTask(task.ID) != nil })

	ctrl.markResumed(task.ID)
	ctrl.Pause(task.ID)
	waitUntil(t, "task to stop", func() bool { return ctrl.manager.GetTask(task.ID) == nil })

	if ctrl.recentlyResumed(task.ID) {
		t.Fatal("a local pause left the resume grace window open")
	}
}

// End-to-end over the real transport: controller → control server → control
// client, i.e. exactly what `unarr downloads` and the tray do. The unit tests
// above check each half; this one catches a wiring mistake between them.
func TestControlPlane_EndToEnd(t *testing.T) {
	ctrl, cancelCtl := newTestController(t)
	defer cancelCtl()

	srv, err := control.NewServer(ctrl)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	client := control.NewClient(srv.Port(), srv.Token())

	task := ctlTask("e2e")
	ctrl.submit(task)
	waitUntil(t, "task to start", func() bool { return ctrl.manager.GetTask(task.ID) != nil })

	tasks, err := client.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 1 || !tasks[0].Running {
		t.Fatalf("List over the wire = %+v", tasks)
	}

	// Stop it by its 8-char prefix, the id the CLI and the logs print.
	results, err := client.Do(context.Background(), control.ActionCancel,
		control.ActionRequest{TaskID: task.ID[:8]})
	if err != nil {
		t.Fatalf("cancel over the wire: %v", err)
	}
	if len(results) != 1 || !results[0].Applied {
		t.Fatalf("cancel results = %+v", results)
	}

	waitUntil(t, "the download to stop", func() bool { return ctrl.manager.GetTask(task.ID) == nil })
	if _, ok := ctrl.store.Get(task.ID); ok {
		t.Fatal("the resume entry survived a cancel issued over the control plane")
	}
}

// The store is on disk, so a restart must see the same queue — that persistence
// is the whole reason a zombie survives one.
func TestActiveTaskStore_SurvivesReload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	store := agent.NewActiveTaskStore()
	store.Add(ctlTask("persisted"))

	reloaded := agent.NewActiveTaskStore()
	tasks := reloaded.Load()
	if len(tasks) != 1 || tasks[0].ID != ctlTask("persisted").ID {
		t.Fatalf("reloaded store = %+v", tasks)
	}

	if n := reloaded.Clear(); n != 1 {
		t.Fatalf("Clear reported %d entries, want 1", n)
	}
	again := agent.NewActiveTaskStore()
	if got := again.Load(); len(got) != 0 {
		t.Fatalf("Clear did not reach disk: %+v", got)
	}

	// Sanity-check the file really is where the docs say it is, since the
	// offline recovery path tells users to delete it by hand.
	if _, err := os.Stat(filepath.Join(dir, "unarr", "active-tasks.json")); err != nil {
		t.Fatalf("active-tasks.json not at the documented path: %v", err)
	}
}
