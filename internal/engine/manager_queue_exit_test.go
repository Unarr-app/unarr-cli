package engine

import (
	"context"
	"testing"
	"time"
)

func (h *queueHarness) tracked(i int) bool {
	r := h.mgr.reporter
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.latest[queueTaskID(i)]
	return ok
}

// Every way out of the line without running must stop the reporter tracking
// the task, or it sits there for the daemon's lifetime.
func TestLeavingTheQueueUntracksTheTask(t *testing.T) {
	h := newQueueHarness(t, 1)
	for i := range 4 {
		h.submit(i)
	}
	waitUntil(t, "task 0 running", func() bool { return len(h.dl.startOrder()) == 1 })
	for i := 1; i < 4; i++ {
		if !h.tracked(i) {
			t.Fatalf("queued task %d should be tracked while it waits", i)
		}
	}

	h.mgr.CancelTask(queueTaskID(1))
	h.mgr.PauseTask(queueTaskID(2))
	h.mgr.CancelAndDeleteFiles(queueTaskID(3))
	for i := 1; i < 4; i++ {
		if h.tracked(i) {
			t.Errorf("task %d left the queue but is still tracked", i)
		}
	}

	h.submit(4)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { h.dl.release <- struct{}{} }()
	h.mgr.Shutdown(sctx)
	if h.tracked(4) {
		t.Error("task drained by Shutdown is still tracked")
	}
}
