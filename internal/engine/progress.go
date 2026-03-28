package engine

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/torrentclaw/torrentclaw-cli/internal/agent"
)

// ActionFunc is called when the server signals an action on a task.
type ActionFunc func(taskID string)

// StatusReporter is the interface used by ProgressReporter to send progress updates.
// Both *agent.Client and agent.Transport implement this via their ReportStatus/SendProgress methods.
type StatusReporter interface {
	ReportStatus(ctx context.Context, update agent.StatusUpdate) (*agent.StatusResponse, error)
}

// ProgressReporter aggregates progress from downloads and reports to the API.
// It batches updates to avoid flooding the server.
type ProgressReporter struct {
	reporter StatusReporter
	interval time.Duration

	onCancel          ActionFunc
	onPause           ActionFunc
	onDeleteFiles     ActionFunc
	onStreamRequested ActionFunc

	mu     sync.Mutex
	latest map[string]*Task // taskID -> task with latest progress
}

// NewProgressReporter creates a reporter that flushes every interval.
// Accepts *agent.Client directly (backwards compatible).
func NewProgressReporter(ac *agent.Client, interval time.Duration) *ProgressReporter {
	return &ProgressReporter{
		reporter: ac,
		interval: interval,
		latest:   make(map[string]*Task),
	}
}

// NewProgressReporterWithTransport creates a reporter using a Transport.
func NewProgressReporterWithTransport(t agent.Transport, interval time.Duration) *ProgressReporter {
	return &ProgressReporter{
		reporter: &transportStatusAdapter{t: t},
		interval: interval,
		latest:   make(map[string]*Task),
	}
}

// transportStatusAdapter adapts agent.Transport to StatusReporter.
type transportStatusAdapter struct {
	t agent.Transport
}

func (a *transportStatusAdapter) ReportStatus(ctx context.Context, update agent.StatusUpdate) (*agent.StatusResponse, error) {
	return a.t.SendProgress(ctx, update)
}

// SetCancelHandler sets the callback invoked when the server says a task is cancelled.
func (r *ProgressReporter) SetCancelHandler(fn ActionFunc) { r.onCancel = fn }

// SetPauseHandler sets the callback invoked when the server says a task is paused.
func (r *ProgressReporter) SetPauseHandler(fn ActionFunc) { r.onPause = fn }

// SetDeleteFilesHandler sets the callback for cancel+delete files.
func (r *ProgressReporter) SetDeleteFilesHandler(fn ActionFunc) { r.onDeleteFiles = fn }

// SetStreamRequestedHandler sets the callback for stream activation.
func (r *ProgressReporter) SetStreamRequestedHandler(fn ActionFunc) { r.onStreamRequested = fn }

// Track registers a task for progress tracking.
func (r *ProgressReporter) Track(task *Task) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latest[task.ID] = task
}

// Untrack removes a task from progress tracking.
func (r *ProgressReporter) Untrack(taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.latest, taskID)
}

// Run starts the periodic flush loop. Blocks until ctx is cancelled.
func (r *ProgressReporter) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.flush(context.Background())
			return nil
		case <-ticker.C:
			r.flush(ctx)
		}
	}
}

func (r *ProgressReporter) flush(ctx context.Context) {
	r.mu.Lock()
	tasks := make([]*Task, 0, len(r.latest))
	for _, t := range r.latest {
		tasks = append(tasks, t)
	}
	r.mu.Unlock()

	for _, task := range tasks {
		status := task.GetStatus()
		if status != StatusDownloading && status != StatusVerifying &&
			status != StatusOrganizing && status != StatusSeeding &&
			status != StatusCompleted && status != StatusFailed {
			continue
		}

		update := task.ToStatusUpdate()
		resp, err := r.reporter.ReportStatus(ctx, update)
		if err != nil {
			log.Printf("[%s] progress report failed: %v", task.ID[:8], err)
			continue
		}

		// Handle server-side signals
		if resp.Cancelled {
			log.Printf("[%s] cancelled by user (via web)", task.ID[:8])
			r.Untrack(task.ID)
			if resp.DeleteFiles && r.onDeleteFiles != nil {
				r.onDeleteFiles(task.ID)
			} else if r.onCancel != nil {
				r.onCancel(task.ID)
			}
		} else if resp.Paused {
			log.Printf("[%s] paused by user (via web)", task.ID[:8])
			r.Untrack(task.ID)
			if r.onPause != nil {
				r.onPause(task.ID)
			}
		}

		if resp.StreamRequested && task.GetStreamURL() == "" {
			log.Printf("[%s] stream requested by user (via web)", task.ID[:8])
			if r.onStreamRequested != nil {
				r.onStreamRequested(task.ID)
			}
		}
	}
}

// ReportFinal sends a final status update for a completed/failed task.
func (r *ProgressReporter) ReportFinal(ctx context.Context, task *Task) {
	update := task.ToStatusUpdate()
	if _, err := r.reporter.ReportStatus(ctx, update); err != nil {
		log.Printf("[%s] final report failed: %v", task.ID[:8], err)
	}
	r.Untrack(task.ID)
}
