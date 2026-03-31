package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/torrentclaw/unarr/internal/agent"
)

// mockStatusReporter records calls to ReportStatus.
type mockStatusReporter struct {
	mu      sync.Mutex
	calls   []agent.StatusUpdate
	resp    *agent.StatusResponse
	respErr error
}

func (m *mockStatusReporter) ReportStatus(_ context.Context, update agent.StatusUpdate) (*agent.StatusResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, update)
	if m.resp != nil {
		return m.resp, m.respErr
	}
	return &agent.StatusResponse{}, m.respErr
}

// mockBatchReporter records batch calls.
type mockBatchReporter struct {
	mockStatusReporter
	batchCalls [][]agent.StatusUpdate
	batchResp  *agent.BatchStatusResponse
}

func (m *mockBatchReporter) BatchReportStatus(_ context.Context, updates []agent.StatusUpdate) (*agent.BatchStatusResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batchCalls = append(m.batchCalls, updates)
	if m.batchResp != nil {
		return m.batchResp, nil
	}
	results := make([]agent.StatusResponse, len(updates))
	return &agent.BatchStatusResponse{Results: results}, nil
}

func TestProgressReporter_TrackUntrack(t *testing.T) {
	reporter := &mockStatusReporter{}
	pr := &ProgressReporter{
		reporter:     reporter,
		interval:     time.Second,
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
	}

	task := &Task{ID: "task-001", Status: StatusDownloading}
	pr.Track(task)

	pr.mu.Lock()
	if _, ok := pr.latest["task-001"]; !ok {
		t.Error("task should be tracked")
	}
	pr.mu.Unlock()

	pr.Untrack("task-001")

	pr.mu.Lock()
	if _, ok := pr.latest["task-001"]; ok {
		t.Error("task should be untracked")
	}
	pr.mu.Unlock()
}

func TestProgressReporter_FlushReportsFinalStates(t *testing.T) {
	reporter := &mockStatusReporter{}
	pr := &ProgressReporter{
		reporter:     reporter,
		interval:     time.Second,
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
	}

	completed := &Task{ID: "task-completed-1234", Status: StatusCompleted}
	pr.Track(completed)

	pr.flush(context.Background())

	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if len(reporter.calls) != 1 {
		t.Fatalf("expected 1 report, got %d", len(reporter.calls))
	}
	if reporter.calls[0].TaskID != "task-completed-1234" {
		t.Errorf("reported wrong task: %s", reporter.calls[0].TaskID)
	}
}

func TestProgressReporter_FlushSkipsWhenNotWatching(t *testing.T) {
	reporter := &mockStatusReporter{}
	pr := &ProgressReporter{
		reporter:     reporter,
		interval:     time.Second,
		isWatching:   func() bool { return false },
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
		lastCheckAt:  time.Now(), // not due for control check
	}

	// Active downloading task, already reported as downloading
	task := &Task{ID: "task-active-12345678", Status: StatusDownloading}
	pr.Track(task)
	pr.mu.Lock()
	pr.lastReported["task-active-12345678"] = StatusDownloading
	pr.mu.Unlock()

	pr.flush(context.Background())

	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if len(reporter.calls) != 0 {
		t.Errorf("expected 0 reports when not watching (no transition), got %d", len(reporter.calls))
	}
}

func TestProgressReporter_FlushReportsTransitions(t *testing.T) {
	reporter := &mockStatusReporter{}
	pr := &ProgressReporter{
		reporter:     reporter,
		interval:     time.Second,
		isWatching:   func() bool { return false },
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
		lastCheckAt:  time.Now(),
	}

	// Task transitioning from resolving to downloading
	task := &Task{ID: "task-trans-12345678", Status: StatusDownloading}
	pr.Track(task)
	pr.mu.Lock()
	pr.lastReported["task-trans-12345678"] = StatusResolving
	pr.mu.Unlock()

	pr.flush(context.Background())

	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if len(reporter.calls) != 1 {
		t.Fatalf("expected 1 report for transition, got %d", len(reporter.calls))
	}
}

func TestProgressReporter_FlushActiveWhenWatching(t *testing.T) {
	reporter := &mockStatusReporter{}
	pr := &ProgressReporter{
		reporter:     reporter,
		interval:     time.Second,
		isWatching:   func() bool { return true },
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
	}

	task := &Task{ID: "task-watch-12345678", Status: StatusDownloading}
	pr.Track(task)
	pr.mu.Lock()
	pr.lastReported["task-watch-12345678"] = StatusDownloading
	pr.mu.Unlock()

	pr.flush(context.Background())

	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if len(reporter.calls) != 1 {
		t.Fatalf("expected 1 report when watching active task, got %d", len(reporter.calls))
	}
}

func TestProgressReporter_HandleResponseCancel(t *testing.T) {
	reporter := &mockStatusReporter{
		resp: &agent.StatusResponse{Cancelled: true},
	}

	var cancelledID string
	pr := &ProgressReporter{
		reporter:     reporter,
		interval:     time.Second,
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
		onCancel:     func(id string) { cancelledID = id },
	}

	task := &Task{ID: "task-cancel-1234567", Status: StatusCompleted}
	pr.Track(task)

	pr.flush(context.Background())

	if cancelledID != "task-cancel-1234567" {
		t.Errorf("expected cancel handler called with task ID, got %q", cancelledID)
	}
}

func TestProgressReporter_HandleResponsePause(t *testing.T) {
	reporter := &mockStatusReporter{
		resp: &agent.StatusResponse{Paused: true},
	}

	var pausedID string
	pr := &ProgressReporter{
		reporter:     reporter,
		interval:     time.Second,
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
		onPause:      func(id string) { pausedID = id },
	}

	task := &Task{ID: "task-paused-1234567", Status: StatusCompleted}
	pr.Track(task)

	pr.flush(context.Background())

	if pausedID != "task-paused-1234567" {
		t.Errorf("expected pause handler called, got %q", pausedID)
	}
}

func TestProgressReporter_HandleResponseDeleteFiles(t *testing.T) {
	reporter := &mockStatusReporter{
		resp: &agent.StatusResponse{Cancelled: true, DeleteFiles: true},
	}

	var deletedID string
	pr := &ProgressReporter{
		reporter:     reporter,
		interval:     time.Second,
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
		onDeleteFiles: func(id string) { deletedID = id },
	}

	task := &Task{ID: "task-delete-1234567", Status: StatusCompleted}
	pr.Track(task)

	pr.flush(context.Background())

	if deletedID != "task-delete-1234567" {
		t.Errorf("expected deleteFiles handler called, got %q", deletedID)
	}
}

func TestProgressReporter_HandleResponseStream(t *testing.T) {
	reporter := &mockStatusReporter{
		resp: &agent.StatusResponse{StreamRequested: true},
	}

	var streamID string
	pr := &ProgressReporter{
		reporter:     reporter,
		interval:     time.Second,
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
		onStreamRequested: func(id string) { streamID = id },
	}

	// Task with no stream URL yet
	task := &Task{ID: "task-stream-1234567", Status: StatusCompleted}
	pr.Track(task)

	pr.flush(context.Background())

	if streamID != "task-stream-1234567" {
		t.Errorf("expected stream handler called, got %q", streamID)
	}
}

func TestProgressReporter_HandleResponseWatchingChanged(t *testing.T) {
	reporter := &mockStatusReporter{
		resp: &agent.StatusResponse{Watching: true},
	}

	var watchingValue bool
	pr := &ProgressReporter{
		reporter:          reporter,
		interval:          time.Second,
		latest:            make(map[string]*Task),
		lastReported:      make(map[string]TaskStatus),
		onWatchingChanged: func(w bool) { watchingValue = w },
	}

	task := &Task{ID: "task-watch2-1234567", Status: StatusCompleted}
	pr.Track(task)

	pr.flush(context.Background())

	if !watchingValue {
		t.Error("expected watchingChanged called with true")
	}
}

func TestProgressReporter_BatchFlush(t *testing.T) {
	batcher := &mockBatchReporter{
		batchResp: &agent.BatchStatusResponse{
			Results: []agent.StatusResponse{{}, {}},
		},
	}

	pr := &ProgressReporter{
		reporter:     batcher,
		interval:     time.Second,
		isWatching:   func() bool { return true },
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
	}

	pr.Track(&Task{ID: "task-batch1-1234567", Status: StatusDownloading})
	pr.Track(&Task{ID: "task-batch2-1234567", Status: StatusDownloading})

	pr.flush(context.Background())

	batcher.mu.Lock()
	defer batcher.mu.Unlock()

	if len(batcher.batchCalls) != 1 {
		t.Fatalf("expected 1 batch call, got %d", len(batcher.batchCalls))
	}
	if len(batcher.batchCalls[0]) != 2 {
		t.Errorf("expected 2 updates in batch, got %d", len(batcher.batchCalls[0]))
	}
}

func TestProgressReporter_RunStopsOnCancel(t *testing.T) {
	reporter := &mockStatusReporter{}
	pr := &ProgressReporter{
		reporter:     reporter,
		interval:     50 * time.Millisecond,
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := pr.Run(ctx)
	if err != nil {
		t.Errorf("Run should return nil on context cancel, got: %v", err)
	}
}

func TestProgressReporter_ReportFinal(t *testing.T) {
	reporter := &mockStatusReporter{}
	pr := &ProgressReporter{
		reporter:     reporter,
		interval:     time.Second,
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
	}

	task := &Task{ID: "task-final-12345678", Status: StatusCompleted}
	pr.Track(task)

	pr.ReportFinal(context.Background(), task)

	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if len(reporter.calls) != 1 {
		t.Fatalf("expected 1 final report, got %d", len(reporter.calls))
	}

	// Should be untracked after final report
	pr.mu.Lock()
	if _, ok := pr.latest["task-final-12345678"]; ok {
		t.Error("task should be untracked after ReportFinal")
	}
	pr.mu.Unlock()
}

func TestProgressReporter_SetHandlers(t *testing.T) {
	pr := &ProgressReporter{
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
	}

	pr.SetCancelHandler(func(id string) {})
	pr.SetPauseHandler(func(id string) {})
	pr.SetDeleteFilesHandler(func(id string) {})
	pr.SetStreamRequestedHandler(func(id string) {})
	pr.SetWatchingFunc(func() bool { return true })
	pr.SetWatchingChangedHandler(func(w bool) {})

	if pr.onCancel == nil || pr.onPause == nil || pr.onDeleteFiles == nil ||
		pr.onStreamRequested == nil || pr.isWatching == nil || pr.onWatchingChanged == nil {
		t.Error("expected all handlers to be set")
	}
}

func TestProgressReporter_ControlCheckDue(t *testing.T) {
	reporter := &mockStatusReporter{}
	pr := &ProgressReporter{
		reporter:     reporter,
		interval:     time.Second,
		isWatching:   func() bool { return false },
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
		lastCheckAt:  time.Now().Add(-31 * time.Second), // 31s ago - due for control check
	}

	task := &Task{ID: "task-ctrl-123456789", Status: StatusDownloading}
	pr.Track(task)
	pr.mu.Lock()
	pr.lastReported["task-ctrl-123456789"] = StatusDownloading
	pr.mu.Unlock()

	pr.flush(context.Background())

	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if len(reporter.calls) != 1 {
		t.Errorf("expected 1 report for control check, got %d", len(reporter.calls))
	}
}
