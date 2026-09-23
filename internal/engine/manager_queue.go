package engine

import (
	"context"
	"log"
	"sync"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// Download-slot queue.
//
// Submit used to wait for a slot on the CALLER's goroutine. Its callers are the
// daemon's startup resume loop and the sync loop, so a full queue parked the
// whole agent: 7 resumed torrents, 5 slots, the first five waiting forever for
// metadata, and the 6th Submit held the main goroutine before the first sync —
// no heartbeat, no streaming, the web showed "unarr sin conexión" while the
// container reported healthy (2026-09-23). Download slots must gate downloads
// and nothing else.
//
// So a task that finds no free slot waits in m.queue — a plain FIFO list, no
// goroutine — and dispatch starts the oldest queued task whenever a slot frees.
// Because a queued task is only data:
//   - cancel / pause / delete take it out of the line synchronously, so the
//     slot it was waiting for is not wasted and a Retry right after a cancel is
//     not swallowed by Submit's dedup;
//   - an ending daemon context or Shutdown never makes it "fail" (and drop its
//     resume entry): it simply never starts;
//   - FreeSlots is exact: running and queued are read under one lock.

// queuedTask is a submitted download waiting for a slot.
type queuedTask struct {
	task   *Task
	ctx    context.Context
	cancel context.CancelFunc
}

// enqueue appends a task to the line and starts whatever now fits.
func (m *Manager) enqueue(q *queuedTask) {
	m.queueMu.Lock()
	m.queue = append(m.queue, q)
	m.queueMu.Unlock()
	m.dispatch()
}

// dispatch starts queued tasks, oldest first, while slots are free. Called on
// every enqueue and every time a running task gives its slot back.
func (m *Manager) dispatch() {
	var dropped []string
	defer func() {
		for _, id := range dropped { // outside queueMu: forgetActive takes other locks
			m.forgetActive(id)
		}
	}()
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	for m.running < m.cfg.MaxConcurrent && len(m.queue) > 0 && !m.shuttingDown.Load() {
		q := m.queue[0]
		m.queue[0] = nil
		m.queue = m.queue[1:]
		if q.ctx.Err() != nil {
			// The daemon context ended without a Shutdown. Never start it, and
			// leave its resume entry alone: the next start picks it up.
			q.cancel()
			dropped = append(dropped, q.task.ID)
			continue
		}
		m.running++
		// wg.Add under queueMu, before the goroutine exists: a finishing task
		// calls dispatch before its own wg.Done, so the counter never touches
		// zero while work is still being handed over.
		m.wg.Add(1)
		go m.runSlot(q)
	}
}

// runSlot downloads a task while holding a slot, then hands the slot on.
func (m *Manager) runSlot(q *queuedTask) {
	defer m.wg.Done()
	lease := &slotLease{m: m, held: true}
	q.task.mu.Lock()
	q.task.slot = lease
	q.task.mu.Unlock()
	defer func() {
		lease.release()
		if m.OnTaskDone != nil {
			m.OnTaskDone()
		}
		m.dispatch()
	}()
	defer q.cancel()
	m.processTask(q.ctx, q.task)
}

// slotLease is a running task's claim on one download slot. A task stalled
// with nothing to download — a torrent that cannot find a single peer to give
// it metadata — yields the slot so the queue behind it moves (the Transmission
// "stalled torrents don't count" rule; 2026-09-23: four dead torrents held four
// of five slots for a week). It keeps running, and reclaims a slot the moment
// it has something to download, even if that briefly puts the count over
// MaxConcurrent: no new task starts until the count is back under.
type slotLease struct {
	m    *Manager
	mu   sync.Mutex
	held bool
}

// yield gives the slot back and lets the next queued task start. Idempotent.
func (l *slotLease) yield() {
	l.mu.Lock()
	was := l.held
	l.held = false
	l.mu.Unlock()
	if !was {
		return
	}
	l.m.queueMu.Lock()
	l.m.running--
	l.m.queueMu.Unlock()
	if l.m.OnTaskDone != nil {
		l.m.OnTaskDone() // advertise the freed slot to the server now
	}
	l.m.dispatch()
}

// reclaim counts the task as running again. Idempotent.
func (l *slotLease) reclaim() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		return
	}
	l.held = true
	l.m.queueMu.Lock()
	l.m.running++
	l.m.queueMu.Unlock()
}

// release is the end of the task: give back the slot if it still holds one.
func (l *slotLease) release() {
	l.mu.Lock()
	was := l.held
	l.held = false
	l.mu.Unlock()
	if was {
		l.m.queueMu.Lock()
		l.m.running--
		l.m.queueMu.Unlock()
	}
}

// YieldSlot lets a downloader stalled with nothing to transfer give its
// download slot to the next queued task while it keeps trying. No-op for a
// force-started task, which holds no slot.
func (t *Task) YieldSlot() {
	t.mu.RLock()
	l := t.slot
	t.mu.RUnlock()
	if l != nil {
		l.yield()
	}
}

// ReclaimSlot counts a previously stalled task as running again, once it has
// something to download. No-op without a lease or when it never yielded.
func (t *Task) ReclaimSlot() {
	t.mu.RLock()
	l := t.slot
	t.mu.RUnlock()
	if l != nil {
		l.reclaim()
	}
}

// dequeue takes a task out of the line if it is still waiting there, and
// forgets it as active. Reports whether it was queued (true) — as opposed to
// running, unknown, or already started by a concurrent dispatch.
func (m *Manager) dequeue(taskID string) bool {
	m.queueMu.Lock()
	idx := -1
	for i, q := range m.queue {
		if q.task.ID == taskID {
			idx = i
			break
		}
	}
	if idx >= 0 {
		m.queue = append(m.queue[:idx], m.queue[idx+1:]...)
	}
	m.queueMu.Unlock()
	if idx < 0 {
		return false
	}
	m.forgetActive(taskID)
	return true
}

// drainQueue empties the line for Shutdown. Queued tasks never started, so there
// is nothing to stop and nothing to report; their resume entries stay put so
// the next start re-submits them.
func (m *Manager) drainQueue() {
	m.queueMu.Lock()
	pending := m.queue
	m.queue = nil
	m.queueMu.Unlock()
	for _, q := range pending {
		q.cancel()
		m.forgetActive(q.task.ID)
	}
	if len(pending) > 0 {
		log.Printf("[shutdown] %d queued download(s) not started - they resume on next start", len(pending))
	}
}

// finishQueued records the final state of a task that a cancel or pause took
// out of the line. The caller already transitioned it (Claimed → Cancelled is
// valid) and settled the resume store BEFORE dequeuing, so this must not touch
// the store again: a resubmit of the same ID may already own a fresh entry.
func (m *Manager) finishQueued(task *Task, how string) {
	log.Printf("[%s] %s while queued: %s", agent.ShortID(task.ID), how, task.Title)
	m.recordFinishedKeep(task.ToStatusUpdate(), true)
}

// forgetActive drops a task that leaves the line without running: it is no
// longer active, and the reporter Submit tracked it with stops tracking it
// (a running task is untracked by its final report instead).
func (m *Manager) forgetActive(taskID string) {
	m.activeMu.Lock()
	delete(m.active, taskID)
	delete(m.cancels, taskID)
	m.activeMu.Unlock()
	m.reporter.Untrack(taskID)
}
