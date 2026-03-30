package engine

import (
	"testing"

	"github.com/torrentclaw/unarr/internal/agent"
)

func TestNewTaskFromAgent(t *testing.T) {
	at := agent.Task{
		ID:              "uuid-123",
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "The Matrix (1999)",
		PreferredMethod: "auto",
	}
	task := NewTaskFromAgent(at)

	if task.ID != "uuid-123" {
		t.Errorf("ID = %q, want uuid-123", task.ID)
	}
	if task.Status != StatusClaimed {
		t.Errorf("Status = %q, want claimed", task.Status)
	}
	if task.ClaimedAt.IsZero() {
		t.Error("ClaimedAt should be set")
	}
}

func TestTransitionValid(t *testing.T) {
	transitions := []struct {
		from TaskStatus
		to   TaskStatus
	}{
		{StatusClaimed, StatusResolving},
		{StatusResolving, StatusDownloading},
		{StatusDownloading, StatusVerifying},
		{StatusVerifying, StatusOrganizing},
		{StatusOrganizing, StatusCompleted},
	}

	for _, tt := range transitions {
		t.Run(string(tt.from)+"->"+string(tt.to), func(t *testing.T) {
			task := &Task{Status: tt.from}
			if err := task.Transition(tt.to); err != nil {
				t.Errorf("valid transition %s -> %s failed: %v", tt.from, tt.to, err)
			}
			if task.Status != tt.to {
				t.Errorf("Status = %q, want %q", task.Status, tt.to)
			}
		})
	}
}

func TestTransitionInvalid(t *testing.T) {
	invalid := []struct {
		from TaskStatus
		to   TaskStatus
	}{
		{StatusPending, StatusDownloading},
		{StatusClaimed, StatusCompleted},
		{StatusCompleted, StatusDownloading},
		{StatusFailed, StatusCompleted},
		{StatusVerifying, StatusResolving},
	}

	for _, tt := range invalid {
		t.Run(string(tt.from)+"->"+string(tt.to), func(t *testing.T) {
			task := &Task{Status: tt.from}
			if err := task.Transition(tt.to); err == nil {
				t.Errorf("invalid transition %s -> %s should fail", tt.from, tt.to)
			}
		})
	}
}

func TestTransitionDownloadingSetsStartedAt(t *testing.T) {
	task := &Task{Status: StatusResolving}
	task.Transition(StatusDownloading)
	if task.StartedAt.IsZero() {
		t.Error("StartedAt should be set on downloading transition")
	}
}

func TestTransitionCompletedSetsCompletedAt(t *testing.T) {
	task := &Task{Status: StatusOrganizing}
	task.Transition(StatusCompleted)
	if task.CompletedAt.IsZero() {
		t.Error("CompletedAt should be set")
	}
}

func TestTransitionFailedSetsCompletedAt(t *testing.T) {
	task := &Task{Status: StatusResolving}
	task.Transition(StatusFailed)
	if task.CompletedAt.IsZero() {
		t.Error("CompletedAt should be set on failure")
	}
}

func TestFallbackTransition(t *testing.T) {
	// downloading -> resolving (fallback)
	task := &Task{Status: StatusDownloading}
	if err := task.Transition(StatusResolving); err != nil {
		t.Errorf("fallback transition should work: %v", err)
	}
}

func TestCancelFromMultipleStates(t *testing.T) {
	for _, from := range []TaskStatus{StatusClaimed, StatusResolving, StatusDownloading} {
		t.Run(string(from), func(t *testing.T) {
			task := &Task{Status: from}
			if err := task.Transition(StatusCancelled); err != nil {
				t.Errorf("cancel from %s should work: %v", from, err)
			}
		})
	}
}

func TestPercent(t *testing.T) {
	task := &Task{DownloadedBytes: 500, TotalBytes: 1000}
	if p := task.Percent(); p != 50 {
		t.Errorf("Percent = %d, want 50", p)
	}

	task2 := &Task{DownloadedBytes: 0, TotalBytes: 0}
	if p := task2.Percent(); p != 0 {
		t.Errorf("Percent = %d, want 0 for zero total", p)
	}
}

func TestUpdateProgress(t *testing.T) {
	task := &Task{}
	task.UpdateProgress(Progress{
		DownloadedBytes: 1024,
		TotalBytes:      2048,
		SpeedBps:        512,
		ETA:             2,
		FileName:        "movie.mkv",
	})
	if task.DownloadedBytes != 1024 {
		t.Errorf("DownloadedBytes = %d", task.DownloadedBytes)
	}
	if task.FileName != "movie.mkv" {
		t.Errorf("FileName = %q", task.FileName)
	}
}

func TestToStatusUpdate(t *testing.T) {
	task := &Task{
		ID:              "task-123",
		Status:          StatusDownloading,
		ResolvedMethod:  MethodTorrent,
		DownloadedBytes: 500,
		TotalBytes:      1000,
		SpeedBps:        100,
		ETA:             5,
		FileName:        "file.mkv",
	}
	update := task.ToStatusUpdate()
	if update.TaskID != "task-123" {
		t.Errorf("TaskID = %q", update.TaskID)
	}
	if update.Status != "downloading" {
		t.Errorf("Status = %q, want downloading", update.Status)
	}
	if update.Progress != 50 {
		t.Errorf("Progress = %d, want 50", update.Progress)
	}
	if update.ResolvedMethod != "torrent" {
		t.Errorf("ResolvedMethod = %q", update.ResolvedMethod)
	}
}

func TestToStatusUpdateGranularStates(t *testing.T) {
	tests := []struct {
		status    TaskStatus
		wantAPI   string
	}{
		{StatusResolving, "resolving"},
		{StatusDownloading, "downloading"},
		{StatusVerifying, "verifying"},
		{StatusOrganizing, "organizing"},
		{StatusCompleted, "completed"},
		{StatusFailed, "failed"},
		{StatusSeeding, "downloading"}, // seeding maps to downloading for backwards compat
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			task := &Task{
				ID:     "task-1",
				Status: tt.status,
			}
			update := task.ToStatusUpdate()
			if update.Status != tt.wantAPI {
				t.Errorf("ToStatusUpdate().Status for %s = %q, want %q", tt.status, update.Status, tt.wantAPI)
			}
		})
	}
}

func TestMagnetURI(t *testing.T) {
	task := &Task{InfoHash: "abc123"}
	m := task.MagnetURI()
	if m != "magnet:?xt=urn:btih:abc123" {
		t.Errorf("MagnetURI = %q", m)
	}
}

func TestHasUntried(t *testing.T) {
	task := &Task{TriedMethods: []DownloadMethod{MethodTorrent}}
	if !task.HasUntried([]DownloadMethod{MethodTorrent, MethodDebrid}) {
		t.Error("should have untried (debrid)")
	}
	if task.HasUntried([]DownloadMethod{MethodTorrent}) {
		t.Error("all methods tried")
	}
}
