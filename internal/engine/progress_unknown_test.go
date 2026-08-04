package engine

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// A task the server no longer knows about is the zombie case: the row was
// deleted (cancel + remove from the list), so no cancel flag and no sync
// control can ever reach this download again. It must be put down locally —
// but only after a few consecutive answers, so a server blip does not kill
// healthy downloads.
func TestHandleResponse_UnknownTaskReapedAfterThreshold(t *testing.T) {
	var mu sync.Mutex
	var reaped []string

	pr := NewProgressReporter(nil, time.Second)
	pr.SetUnknownTaskHandler(func(id string) {
		mu.Lock()
		defer mu.Unlock()
		reaped = append(reaped, id)
	})

	task := &Task{ID: "task-zombie-0001", Status: StatusDownloading}
	pr.Track(task)

	for i := 1; i < unknownTaskThreshold; i++ {
		pr.handleResponse(task, &agent.StatusResponse{Success: false})
		mu.Lock()
		got := len(reaped)
		mu.Unlock()
		if got != 0 {
			t.Fatalf("reaped after %d unknown answers, want to wait for %d", i, unknownTaskThreshold)
		}
	}

	pr.handleResponse(task, &agent.StatusResponse{Success: false})
	mu.Lock()
	defer mu.Unlock()
	if len(reaped) != 1 || reaped[0] != task.ID {
		t.Fatalf("expected the task to be reaped once, got %v", reaped)
	}
}

// A single failure in a run of successes must not accumulate: a server that
// answers "unknown" once, then normally, is a blip, not a deleted task.
func TestHandleResponse_UnknownStreakResetsOnSuccess(t *testing.T) {
	var reaped int
	pr := NewProgressReporter(nil, time.Second)
	pr.SetUnknownTaskHandler(func(string) { reaped++ })

	task := &Task{ID: "task-flaky-0001", Status: StatusDownloading}
	pr.Track(task)

	for i := 0; i < unknownTaskThreshold*3; i++ {
		pr.handleResponse(task, &agent.StatusResponse{Success: false})
		pr.handleResponse(task, &agent.StatusResponse{Success: true})
	}

	if reaped != 0 {
		t.Fatalf("a task answered successfully between failures was reaped %d time(s)", reaped)
	}
}

// The cancel flag must reach the handler — the regression this whole change
// exists for. Before the fix the handler was never wired in production, so a
// web cancel logged a line, untracked the task, and left the download running.
func TestHandleResponse_CancelCallsHandler(t *testing.T) {
	var cancelled, deleted, paused []string

	pr := NewProgressReporter(nil, time.Second)
	pr.SetCancelHandler(func(id string) { cancelled = append(cancelled, id) })
	pr.SetDeleteFilesHandler(func(id string) { deleted = append(deleted, id) })
	pr.SetPauseHandler(func(id string) { paused = append(paused, id) })

	plain := &Task{ID: "task-cancel-0001", Status: StatusDownloading}
	withFiles := &Task{ID: "task-cancel-0002", Status: StatusDownloading}
	toPause := &Task{ID: "task-pause-0003", Status: StatusDownloading}

	pr.handleResponse(plain, &agent.StatusResponse{Success: true, Cancelled: true})
	pr.handleResponse(withFiles, &agent.StatusResponse{Success: true, Cancelled: true, DeleteFiles: true})
	pr.handleResponse(toPause, &agent.StatusResponse{Success: true, Paused: true})

	if len(cancelled) != 1 || cancelled[0] != plain.ID {
		t.Errorf("cancel handler: got %v", cancelled)
	}
	if len(deleted) != 1 || deleted[0] != withFiles.ID {
		t.Errorf("delete-files handler: got %v", deleted)
	}
	if len(paused) != 1 || paused[0] != toPause.ID {
		t.Errorf("pause handler: got %v", paused)
	}
}

// notFoundReporter answers 404 (the single-report shape of "no such task").
type notFoundReporter struct{ status int }

func (r *notFoundReporter) ReportStatus(context.Context, agent.StatusUpdate) (*agent.StatusResponse, error) {
	return nil, &agent.HTTPError{StatusCode: r.status, Message: "not_found"}
}

// In single-report mode an unknown task comes back as a 404 rather than a
// success=false body. Same zombie, and it must reach the same reaper.
func TestFlush_NotFoundCountsAsUnknown(t *testing.T) {
	var reaped []string
	pr := NewProgressReporter(nil, time.Second)
	pr.reporter = &notFoundReporter{status: http.StatusNotFound}
	pr.SetUnknownTaskHandler(func(id string) { reaped = append(reaped, id) })

	task := &Task{ID: "task-404-000001", Status: StatusDownloading}
	pr.Track(task)

	for i := 0; i < unknownTaskThreshold; i++ {
		pr.Track(task) // Untrack on reap; re-track so the next flush still reports
		pr.flush(context.Background())
	}

	if len(reaped) != 1 {
		t.Fatalf("expected exactly one reap from repeated 404s, got %d", len(reaped))
	}
}

// A 5xx is the server having a bad day, NOT a missing task. Reaping on it
// would stop healthy downloads across the fleet during an outage.
func TestFlush_ServerErrorIsNotUnknown(t *testing.T) {
	var reaped []string
	pr := NewProgressReporter(nil, time.Second)
	pr.reporter = &notFoundReporter{status: http.StatusInternalServerError}
	pr.SetUnknownTaskHandler(func(id string) { reaped = append(reaped, id) })

	task := &Task{ID: "task-500-000001", Status: StatusDownloading}
	pr.Track(task)

	for i := 0; i < unknownTaskThreshold*2; i++ {
		pr.flush(context.Background())
	}

	if len(reaped) != 0 {
		t.Fatalf("server errors must never reap a task, got %v", reaped)
	}
}

func TestHTTPErrorIsMatchable(t *testing.T) {
	// Guards the errors.As in flush(): if HTTPError ever stops being returned
	// as a pointer, the 404 path silently degrades to "report failed".
	var target *agent.HTTPError
	err := error(&agent.HTTPError{StatusCode: 404})
	if !errors.As(err, &target) {
		t.Fatal("agent.HTTPError is no longer matchable with errors.As")
	}
}
