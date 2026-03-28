package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/torrentclaw/torrentclaw-cli/internal/config"
)

// DaemonState is written to disk every heartbeat for external tools to read.
type DaemonState struct {
	AgentID         string         `json:"agentId"`
	Status          string         `json:"status"` // running | upgrading | shutting_down
	Version         string         `json:"version"`
	PID             int            `json:"pid"`
	StartedAt       time.Time      `json:"startedAt"`
	LastHeartbeat   time.Time      `json:"lastHeartbeat"`
	ActiveTasks     int            `json:"activeTasks"`
	CompletedCount  int            `json:"completedCount"`
	FailedCount     int            `json:"failedCount"`
	TotalDownloaded int64          `json:"totalDownloaded"`
	MethodStats     map[string]int `json:"methodStats,omitempty"`
}

// stateFilePathFn is overridable for testing.
var stateFilePathFn = func() string {
	return filepath.Join(config.DataDir(), "daemon.state.json")
}

// StateFilePath returns the path to the daemon state file.
func StateFilePath() string {
	return stateFilePathFn()
}

// WriteState writes the daemon state to disk (best-effort, never errors).
func WriteState(state *DaemonState) {
	path := StateFilePath()
	dir := filepath.Dir(path)
	os.MkdirAll(dir, 0o755)

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}

	// Write to temp file then rename for atomicity
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	os.Rename(tmp, path)
}

// ReadState reads the daemon state from disk. Returns nil if not found.
func ReadState() *DaemonState {
	data, err := os.ReadFile(StateFilePath())
	if err != nil {
		return nil
	}
	var state DaemonState
	if json.Unmarshal(data, &state) != nil {
		return nil
	}
	return &state
}

// RemoveState deletes the state file (called on clean shutdown).
func RemoveState() {
	os.Remove(StateFilePath())
}
