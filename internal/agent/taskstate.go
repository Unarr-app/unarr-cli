package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/torrentclaw/unarr/internal/config"
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

// LocalState holds the CLI's local execution state (tasks.json).
// This is the CLI's source of truth for what it's doing right now.
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

// taskStateFilePathFn is overridable for testing.
var taskStateFilePathFn = func() string {
	return filepath.Join(config.DataDir(), "tasks.json")
}

// WriteToDisk persists local state to disk atomically (best-effort).
func (s *LocalState) WriteToDisk() {
	tasks := s.Snapshot()
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return
	}
	path := taskStateFilePathFn()
	dir := filepath.Dir(path)
	os.MkdirAll(dir, 0o755)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	os.Rename(tmp, path)
}

// ReadFromDisk loads local state from disk. Returns empty state on error.
func (s *LocalState) ReadFromDisk() {
	data, err := os.ReadFile(taskStateFilePathFn())
	if err != nil {
		return
	}
	var tasks []TaskState
	if json.Unmarshal(data, &tasks) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = make(map[string]*TaskState, len(tasks))
	for i := range tasks {
		s.tasks[tasks[i].TaskID] = &tasks[i]
	}
}
