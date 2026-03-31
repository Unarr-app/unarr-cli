package engine

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/torrentclaw/unarr/internal/agent"
)

// ActionFunc is called when the server signals an action on a task.
type ActionFunc func(taskID string)

// StatusReporter is the interface used by ProgressReporter to send progress updates.
// Both *agent.Client and agent.Transport implement this via their ReportStatus/SendProgress methods.
type StatusReporter interface {
	ReportStatus(ctx context.Context, update agent.StatusUpdate) (*agent.StatusResponse, error)
}

// BatchStatusReporter extends StatusReporter with batch support.
// Transports that implement this send all updates in a single request.
type BatchStatusReporter interface {
	StatusReporter
	BatchReportStatus(ctx context.Context, updates []agent.StatusUpdate) (*agent.BatchStatusResponse, error)
}

// WatchingFunc returns whether a user is actively viewing download progress.
type WatchingFunc func() bool

// ProgressReporter aggregates progress from downloads and reports to the API.
// It batches updates to avoid flooding the server.
type ProgressReporter struct {
	reporter   StatusReporter
	interval   time.Duration
	isWatching WatchingFunc // nil = always report (backwards compatible)

	onCancel          ActionFunc
	onPause           ActionFunc
	onDeleteFiles     ActionFunc
	onStreamRequested ActionFunc
	onWatchingChanged func(watching bool)

	mu             sync.Mutex
	latest         map[string]*Task      // taskID -> task with latest progress
	lastReported   map[string]TaskStatus // taskID -> last status sent to API
	lastCheckAt    time.Time             // last time we reported for control-signal polling
}

// NewProgressReporter creates a reporter that flushes every interval.
// Accepts *agent.Client directly (backwards compatible).
func NewProgressReporter(ac *agent.Client, interval time.Duration) *ProgressReporter {
	return &ProgressReporter{
		reporter:     ac,
		interval:     interval,
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
	}
}

// NewProgressReporterWithTransport creates a reporter using a Transport.
func NewProgressReporterWithTransport(t agent.Transport, interval time.Duration) *ProgressReporter {
	return &ProgressReporter{
		reporter:     &transportStatusAdapter{t: t},
		interval:     interval,
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
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

// SetWatchingFunc sets the function that checks if someone is viewing downloads.
func (r *ProgressReporter) SetWatchingFunc(fn WatchingFunc) { r.isWatching = fn }

// SetWatchingChangedHandler sets a callback invoked when the server's watching flag changes.
// This allows the daemon to update its Watching state from status responses (not just heartbeats).
func (r *ProgressReporter) SetWatchingChangedHandler(fn func(watching bool)) {
	r.onWatchingChanged = fn
}

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
	delete(r.lastReported, taskID)
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
	// Snapshot lastReported under the same lock
	lastReported := make(map[string]TaskStatus, len(r.lastReported))
	for k, v := range r.lastReported {
		lastReported[k] = v
	}
	r.mu.Unlock()

	// When nobody is watching, only report final states, status transitions,
	// and periodic check-ins (every 30s) so we still receive control signals
	// (cancel/pause) from the server.
	watching := r.isWatching == nil || r.isWatching()
	controlCheckDue := time.Since(r.lastCheckAt) >= 30*time.Second

	var reportable []*Task
	for _, task := range tasks {
		status := task.GetStatus()
		isFinal := status == StatusCompleted || status == StatusFailed
		isActive := status == StatusDownloading || status == StatusVerifying ||
			status == StatusOrganizing || status == StatusSeeding ||
			status == StatusResolving
		// Always report status transitions so the DB reflects the current state.
		prev := lastReported[task.ID]
		isTransition := prev == "" || prev != status
		if isFinal || isTransition || (watching && isActive) || (controlCheckDue && isActive) {
			reportable = append(reportable, task)
		}
	}

	if controlCheckDue {
		r.lastCheckAt = time.Now()
	}

	if len(reportable) == 0 {
		return
	}

	// Use batch when transport supports it
	if batcher, ok := r.reporter.(BatchStatusReporter); ok {
		r.flushBatch(ctx, batcher, reportable)
		return
	}

	// Fallback: individual requests
	for _, task := range reportable {
		statusAtReport := task.GetStatus() // capture before HTTP round-trip
		update := task.ToStatusUpdate()
		resp, err := r.reporter.ReportStatus(ctx, update)
		if err != nil {
			log.Printf("[%s] progress report failed: %v", task.ID[:8], err)
			continue
		}
		r.mu.Lock()
		r.lastReported[task.ID] = statusAtReport
		r.mu.Unlock()
		r.handleResponse(task, resp)
	}
}

func (r *ProgressReporter) flushBatch(ctx context.Context, batcher BatchStatusReporter, tasks []*Task) {
	updates := make([]agent.StatusUpdate, len(tasks))
	// Capture status before HTTP round-trip to avoid missed transitions
	statusAtReport := make([]TaskStatus, len(tasks))
	for i, task := range tasks {
		updates[i] = task.ToStatusUpdate()
		statusAtReport[i] = task.GetStatus()
	}

	resp, err := batcher.BatchReportStatus(ctx, updates)
	if err != nil {
		log.Printf("batch progress report failed: %v", err)
		return
	}

	// Propagate watching flag from batch response
	if resp.Watching && r.onWatchingChanged != nil {
		r.onWatchingChanged(true)
	}

	// Match results back to tasks by index (server returns in same order)
	if len(resp.Results) != len(tasks) {
		log.Printf("batch response mismatch: sent %d updates, got %d results", len(tasks), len(resp.Results))
	}
	r.mu.Lock()
	for i, task := range tasks {
		r.lastReported[task.ID] = statusAtReport[i]
	}
	r.mu.Unlock()
	for i, result := range resp.Results {
		if i < len(tasks) {
			r.handleResponse(tasks[i], &result)
		}
	}
}

func (r *ProgressReporter) handleResponse(task *Task, resp *agent.StatusResponse) {
	// Propagate watching flag from status response to daemon
	if resp.Watching && r.onWatchingChanged != nil {
		r.onWatchingChanged(true)
	}

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

// ReportFinal sends a final status update for a completed/failed task.
func (r *ProgressReporter) ReportFinal(ctx context.Context, task *Task) {
	update := task.ToStatusUpdate()
	if _, err := r.reporter.ReportStatus(ctx, update); err != nil {
		log.Printf("[%s] final report failed: %v", task.ID[:8], err)
	}
	r.Untrack(task.ID)
}
