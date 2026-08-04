package cmd

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/control"
	"github.com/Unarr-app/unarr-cli/internal/engine"
)

// taskController is the ONE place a download is paused, resumed, cancelled or
// retried, whoever asked. Three callers converge here:
//
//   - the web, through the sync control signals (Daemon.OnControlAction);
//   - the web, through the flags on a status report (ProgressReporter handlers);
//   - the machine itself, through the local control plane (`unarr downloads`,
//     the desktop tray).
//
// They used to be three separate half-implementations, and one of them did
// nothing at all: the ProgressReporter cancel/pause/deleteFiles/stream handlers
// were never wired outside tests, so a cancel that arrived on a status response
// logged "cancelled by user (via web)", stopped REPORTING the task, and let the
// download run on — invisible to the web and unstoppable from it (user report
// 2026-08-03: a 56.8 GB torrent that survived cancel, row deletion and several
// restarts). A single controller is what keeps the three paths honest.
type taskController struct {
	manager *engine.Manager
	store   *agent.ActiveTaskStore
	client  *agent.Client
	agentID string

	// resumedMu guards resumedAt: when this machine last resumed a task on its
	// own. See resumeGrace.
	resumedMu sync.Mutex
	resumedAt map[string]time.Time

	// submit re-runs a task payload (resume/retry). Wired to manager.Submit
	// bound to the daemon context.
	submit func(agent.Task)
	// stopStream tears down any stream session attached to a task.
	stopStream func(taskID string)
	// triggerSync asks for an immediate sync so the web sees the change without
	// waiting for the next tick.
	triggerSync func()
}

// controlReportTimeout bounds the best-effort report of a local action to the
// web. The local action has already happened; this is only the notification.
const controlReportTimeout = 10 * time.Second

// resumeGrace is how long a locally resumed task ignores an incoming pause.
//
// Pause lives on the task row, and the server keeps re-sending it — over the
// sync controls AND over the SSE downlink — for as long as the row says
// "paused". A local resume therefore races its own notification: the agent
// restarts the download, and a pause that was already in flight (or produced
// from the not-yet-updated row) stops it again, forever. Observed directly:
// `unarr downloads resume` followed by six `downlink: control pause on task
// 874fd548` in the same second, with the task never leaving "paused".
//
// So a resume wins for a few seconds. Long enough to cover the report round
// trip plus a sync cycle; short enough that a genuine pause the user asks for
// right after a resume still lands.
const resumeGrace = 20 * time.Second

// List merges what is running with what is merely persisted. A resume-store
// entry with no live task is either a pause or a leftover, and both belong in
// `unarr downloads` — the leftovers especially, since they are the thing the
// user is trying to kill.
func (c *taskController) List() []control.TaskInfo {
	running := make(map[string]control.TaskInfo)
	for _, t := range c.manager.ActiveTasks() {
		u := t.ToStatusUpdate()
		state := string(t.GetStatus())
		running[t.ID] = control.TaskInfo{
			ID:              t.ID,
			Title:           t.Title,
			State:           state,
			Progress:        u.Progress,
			DownloadedBytes: u.DownloadedBytes,
			TotalBytes:      u.TotalBytes,
			SpeedBps:        u.SpeedBps,
			ETA:             u.ETA,
			Method:          u.ResolvedMethod,
			FileName:        u.FileName,
			ErrorMessage:    u.ErrorMessage,
			Running:         true,
		}
	}

	paused := make(map[string]bool)
	for _, id := range c.manager.PausedTaskIDs() {
		paused[id] = true
	}

	out := make([]control.TaskInfo, 0, len(running))
	if c.store != nil {
		for _, t := range c.store.Snapshot() {
			if info, ok := running[t.ID]; ok {
				info.Persisted = true
				running[t.ID] = info
				continue
			}
			state := "stopped"
			if paused[t.ID] {
				state = "paused"
			}
			out = append(out, control.TaskInfo{
				ID:        t.ID,
				Title:     t.Title,
				State:     state,
				Persisted: true,
			})
		}
	}
	for _, info := range running {
		out = append(out, info)
	}
	return out
}

// Pause stops the download but keeps everything needed to continue it: the
// partial files, and the resume-store entry (see Manager.PauseTask).
func (c *taskController) Pause(taskID string) control.ActionResult {
	res := control.ActionResult{TaskID: taskID, Title: c.titleOf(taskID)}
	if !c.manager.PauseTask(taskID) {
		res.Message = "not running"
		return res
	}
	// The task is stopped on purpose now, so the resume grace window has served
	// its purpose. Dropping it keeps a later web pause from being mistaken for a
	// stale echo — and keeps the map from carrying every task ever resumed.
	c.clearResumed(taskID)
	c.stopStreamFor(taskID)
	c.report(taskID, control.ActionPause, false, "paused locally")
	res.Applied = true
	res.Message = "paused"
	return res
}

// Resume re-submits a paused task from its stored payload. When the payload is
// gone (an old pause, or a task that only ever lived on the server) it asks the
// web to re-dispatch instead, which is the only thing left that can produce it.
func (c *taskController) Resume(taskID string) control.ActionResult {
	res := control.ActionResult{TaskID: taskID, Title: c.titleOf(taskID)}
	if c.manager.GetTask(taskID) != nil {
		res.Message = "already running"
		return res
	}

	// Claim the resume BEFORE anything else: an incoming pause produced from the
	// still-"paused" row would otherwise stop the download again the moment it
	// starts. See resumeGrace.
	c.markResumed(taskID)

	if c.store != nil {
		if task, ok := c.store.Get(taskID); ok {
			// Report SYNCHRONOUSLY, then start. The other way round, the server
			// keeps handing out "pause" for this task until the row catches up,
			// and the download it just started is the thing that gets paused.
			c.reportNow(taskID, control.ActionResume, false, "resumed locally")
			task.ForceStart = false // respect MaxConcurrent, like the boot resume
			c.submit(task)
			res.Applied = true
			res.Message = "resumed"
			return res
		}
	}

	// Nothing to run locally — the server owns the payload.
	c.reportNow(taskID, control.ActionResume, false, "resume requested locally")
	c.triggerSync()
	res.Applied = true
	res.Message = "asked the server to re-queue it"
	return res
}

// markResumed / recentlyResumed implement the grace window that keeps a stale
// pause from undoing a resume the user just asked for.
func (c *taskController) markResumed(taskID string) {
	c.resumedMu.Lock()
	defer c.resumedMu.Unlock()
	if c.resumedAt == nil {
		c.resumedAt = make(map[string]time.Time)
	}
	c.resumedAt[taskID] = time.Now()
}

func (c *taskController) recentlyResumed(taskID string) bool {
	c.resumedMu.Lock()
	defer c.resumedMu.Unlock()
	at, ok := c.resumedAt[taskID]
	if !ok {
		return false
	}
	if time.Since(at) > resumeGrace {
		delete(c.resumedAt, taskID) // expired: stop carrying it around
		return false
	}
	return true
}

// clearResumed drops the grace window — used when the task genuinely stops, so
// a later pause is honoured normally.
func (c *taskController) clearResumed(taskID string) {
	c.resumedMu.Lock()
	defer c.resumedMu.Unlock()
	delete(c.resumedAt, taskID)
}

// Cancel stops a download for good. It is deliberately tolerant: a task that is
// not running but IS in the resume store still gets cancelled, because that is
// exactly the zombie case — the store entry is what resurrects the download on
// the next start, so dropping it is the whole point.
func (c *taskController) Cancel(taskID string, deleteFiles bool) control.ActionResult {
	res := control.ActionResult{TaskID: taskID, Title: c.titleOf(taskID)}

	// Terminal: the grace window goes with it, or a task cancelled seconds after
	// a resume would keep ignoring pauses for a download that no longer exists.
	c.clearResumed(taskID)

	stopped := false
	if deleteFiles {
		stopped = c.manager.CancelAndDeleteFiles(taskID)
	} else {
		stopped = c.manager.CancelTask(taskID)
	}

	persisted := false
	if c.store != nil {
		_, persisted = c.store.Get(taskID)
	}
	// CancelTask already drops the entry for a running task; this covers the
	// not-running zombie.
	c.manager.DropResume(taskID)

	if !stopped && !persisted {
		res.Message = "not found"
		return res
	}

	c.stopStreamFor(taskID)
	c.report(taskID, control.ActionCancel, deleteFiles, "cancelled locally")
	res.Applied = true
	switch {
	case stopped && deleteFiles:
		res.Message = "cancelled, partial files deleted"
	case stopped:
		res.Message = "cancelled"
	default:
		res.Message = "removed from the resume queue (was not running)"
	}
	return res
}

// Retry stops whatever is happening and starts the task again from its stored
// payload — the local counterpart of the web's Retry button.
func (c *taskController) Retry(taskID string) control.ActionResult {
	res := control.ActionResult{TaskID: taskID, Title: c.titleOf(taskID)}

	var payload agent.Task
	havePayload := false
	if c.store != nil {
		payload, havePayload = c.store.Get(taskID)
	}

	c.manager.CancelTask(taskID)
	c.stopStreamFor(taskID)

	// A retry restarts a task whose row may well say "paused" or "cancelled",
	// and the server re-sends those controls until the row changes. Same race
	// Resume hits, same answer: claim the window, then report BEFORE starting,
	// so the download that comes back is not stopped again on arrival.
	c.markResumed(taskID)

	if !havePayload {
		// Ask the server to re-dispatch: it still has the metadata we lack.
		c.reportNow(taskID, control.ActionRetry, false, "retry requested locally")
		c.triggerSync()
		res.Applied = true
		res.Message = "asked the server to retry it"
		return res
	}

	c.reportNow(taskID, control.ActionRetry, false, "retried locally")
	payload.ForceStart = true // an explicit retry is fresh user intent
	c.submit(payload)
	res.Applied = true
	res.Message = "restarted"
	return res
}

// Purge drops resume-store entries for tasks that are NOT running. This is the
// "stop resurrecting downloads I already killed" button: entries whose server
// row is gone can never be reconciled, and until they are dropped every daemon
// start re-submits them.
func (c *taskController) Purge() []control.ActionResult {
	if c.store == nil {
		return nil
	}
	var out []control.ActionResult
	for _, t := range c.store.Snapshot() {
		if c.manager.GetTask(t.ID) != nil {
			continue // running: purging it would strand a live download
		}
		c.manager.DropResume(t.ID)
		out = append(out, control.ActionResult{
			TaskID:  t.ID,
			Title:   t.Title,
			Applied: true,
			Message: "dropped from the resume queue",
		})
	}
	return out
}

// ReapUnknown is what the progress reporter calls when the server has answered
// "no such task" repeatedly: stop the download AND drop the resume entry, or
// the next daemon start brings it straight back.
func (c *taskController) ReapUnknown(taskID string) {
	title := c.titleOf(taskID)
	c.manager.CancelTask(taskID)
	c.manager.DropResume(taskID)
	c.stopStreamFor(taskID)
	log.Printf("[%s] dropped: the server no longer has this task (%s)", agent.ShortID(taskID), title)
	// No report: there is no row left to report against.
}

// ApplyWebAction routes a control signal that came from the web (sync control
// list, or a flag on a status response) through the same code path as a local
// one. Reporting back is skipped — the web is where this originated.
func (c *taskController) ApplyWebAction(action, taskID string, deleteFiles bool) {
	switch action {
	case control.ActionCancel:
		// A cancel always wins, even inside the resume grace window: the user
		// asking for a download to end outranks one they asked to continue.
		c.clearResumed(taskID)
		if deleteFiles {
			c.manager.CancelAndDeleteFiles(taskID)
		} else {
			c.manager.CancelTask(taskID)
		}
		c.manager.DropResume(taskID)
		c.stopStreamFor(taskID)
	case control.ActionPause:
		// Ignore a pause that is really an echo of the state this task was in
		// before it was resumed here — the server keeps re-sending pause while
		// the row says "paused", which would undo the resume immediately and
		// forever. See resumeGrace.
		if c.recentlyResumed(taskID) {
			log.Printf("[%s] ignoring pause: resumed on this machine moments ago (the server has not caught up yet)",
				agent.ShortID(taskID))
			return
		}
		c.manager.PauseTask(taskID)
		c.stopStreamFor(taskID)
	case control.ActionResume:
		log.Printf("[%s] resume requested, triggering sync", agent.ShortID(taskID))
		c.markResumed(taskID)
		c.triggerSync()
	}
}

func (c *taskController) stopStreamFor(taskID string) {
	if c.stopStream != nil {
		c.stopStream(taskID)
	}
}

func (c *taskController) titleOf(taskID string) string {
	if t := c.manager.GetTask(taskID); t != nil {
		return t.Title
	}
	if c.store != nil {
		if t, ok := c.store.Get(taskID); ok {
			return t.Title
		}
	}
	return ""
}

// report tells the web about a locally initiated action, off the caller's
// goroutine. Fire-and-forget: the action already happened locally, so a failure
// here must not surface as an error — it only means the web catches up on the
// next sync.
func (c *taskController) report(taskID, action string, deleteFiles bool, reason string) {
	if c.client == nil {
		return
	}
	go c.reportNow(taskID, action, deleteFiles, reason)
}

// reportNow is the same notification, but blocking.
//
// Resume uses this on purpose: the row is what the server generates control
// signals from, so until it stops saying "paused" the agent keeps being told to
// pause the download it is about to start. Waiting out one bounded round trip
// is cheaper than the alternative, which is a resume that never sticks.
func (c *taskController) reportNow(taskID, action string, deleteFiles bool, reason string) {
	if c.client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlReportTimeout)
	defer cancel()
	err := c.client.ReportTaskControl(ctx, agent.TaskControlRequest{
		TaskID:      taskID,
		AgentID:     c.agentID,
		Action:      action,
		DeleteFiles: deleteFiles,
		Reason:      reason,
	})
	if err != nil {
		log.Printf("[%s] could not tell the web about the local %s: %v",
			agent.ShortID(taskID), action, err)
		return
	}
	// Nudge the sync loop so the dashboard refreshes now, not in 30s.
	if c.triggerSync != nil {
		c.triggerSync()
	}
}
