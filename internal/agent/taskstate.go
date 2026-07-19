package agent

import (
	"sync"
	"time"
)

// TaskState represents the execution state of a single download task.
// Written by the Task Engine, read by the Sync goroutine.
type TaskState struct {
	TaskID          string `json:"taskId"`
	Status          string `json:"status"` // resolving, downloading, verifying, organizing, completed, failed
	Progress        int    `json:"progress"`
	DownloadedBytes int64  `json:"downloadedBytes,omitempty"`
	TotalBytes      int64  `json:"totalBytes,omitempty"`
	SpeedBps        int64  `json:"speedBps,omitempty"`
	ETA             int    `json:"eta,omitempty"`
	ResolvedMethod  string `json:"resolvedMethod,omitempty"`
	FileName        string `json:"fileName,omitempty"`
	FilePath        string `json:"filePath,omitempty"`
	StreamURL       string `json:"streamUrl,omitempty"`
	ErrorMessage    string `json:"errorMessage,omitempty"`
	UpdatedAt       int64  `json:"updatedAt"`
}

// LocalState holds the CLI's local execution state in memory. It is the source
// of truth for what the daemon is doing right now and is snapshotted into each
// sync request (sync.go); it is never persisted to disk.
type LocalState struct {
	mu    sync.RWMutex
	tasks map[string]*TaskState
}

// NewLocalState creates an empty local state.
func NewLocalState() *LocalState {
	return &LocalState{
		tasks: make(map[string]*TaskState),
	}
}

// Update adds or updates a task in local state.
func (s *LocalState) Update(ts TaskState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts.UpdatedAt = time.Now().Unix()
	copied := ts
	s.tasks[ts.TaskID] = &copied
}

// Remove removes a task from local state.
func (s *LocalState) Remove(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, taskID)
}

// Snapshot returns a copy of all current task states.
func (s *LocalState) Snapshot() []TaskState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]TaskState, 0, len(s.tasks))
	for _, ts := range s.tasks {
		result = append(result, *ts)
	}
	return result
}

// TaskStateFromUpdate converts a StatusUpdate into a TaskState.
func TaskStateFromUpdate(u StatusUpdate) TaskState {
	return TaskState{
		TaskID:          u.TaskID,
		Status:          u.Status,
		Progress:        u.Progress,
		DownloadedBytes: u.DownloadedBytes,
		TotalBytes:      u.TotalBytes,
		SpeedBps:        u.SpeedBps,
		ETA:             u.ETA,
		ResolvedMethod:  u.ResolvedMethod,
		FileName:        u.FileName,
		FilePath:        u.FilePath,
		StreamURL:       u.StreamURL,
		ErrorMessage:    u.ErrorMessage,
	}
}

// ShortID returns the first 8 characters of an ID, or the full ID if shorter.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
