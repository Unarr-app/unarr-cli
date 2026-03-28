package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteAndReadState(t *testing.T) {
	// Override the state file path for testing
	tmpDir := t.TempDir()
	origFn := stateFilePathFn
	stateFilePathFn = func() string { return filepath.Join(tmpDir, "daemon.state.json") }
	defer func() { stateFilePathFn = origFn }()

	state := &DaemonState{
		AgentID:         "agent-123",
		Status:          "running",
		Version:         "1.0.0",
		PID:             12345,
		StartedAt:       time.Now().Truncate(time.Second),
		LastHeartbeat:   time.Now().Truncate(time.Second),
		ActiveTasks:     3,
		CompletedCount:  10,
		FailedCount:     2,
		TotalDownloaded: 1024 * 1024 * 500,
		MethodStats:     map[string]int{"torrent": 8, "debrid": 2},
	}

	WriteState(state)

	read := ReadState()
	if read == nil {
		t.Fatal("ReadState() returned nil")
	}
	if read.AgentID != "agent-123" {
		t.Errorf("AgentID = %q, want agent-123", read.AgentID)
	}
	if read.Status != "running" {
		t.Errorf("Status = %q, want running", read.Status)
	}
	if read.Version != "1.0.0" {
		t.Errorf("Version = %q, want 1.0.0", read.Version)
	}
	if read.PID != 12345 {
		t.Errorf("PID = %d, want 12345", read.PID)
	}
	if read.ActiveTasks != 3 {
		t.Errorf("ActiveTasks = %d, want 3", read.ActiveTasks)
	}
	if read.CompletedCount != 10 {
		t.Errorf("CompletedCount = %d, want 10", read.CompletedCount)
	}
	if read.MethodStats["torrent"] != 8 {
		t.Errorf("MethodStats[torrent] = %d, want 8", read.MethodStats["torrent"])
	}
}

func TestReadStateNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	origFn := stateFilePathFn
	stateFilePathFn = func() string { return filepath.Join(tmpDir, "nonexistent.json") }
	defer func() { stateFilePathFn = origFn }()

	state := ReadState()
	if state != nil {
		t.Errorf("ReadState() = %+v, want nil for missing file", state)
	}
}

func TestRemoveState(t *testing.T) {
	tmpDir := t.TempDir()
	origFn := stateFilePathFn
	stateFilePathFn = func() string { return filepath.Join(tmpDir, "daemon.state.json") }
	defer func() { stateFilePathFn = origFn }()

	WriteState(&DaemonState{AgentID: "test"})

	// Verify file exists
	path := StateFilePath()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file should exist: %v", err)
	}

	RemoveState()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("state file should be removed after RemoveState()")
	}
}

func TestReadStateCorruptedJSON(t *testing.T) {
	tmpDir := t.TempDir()
	origFn := stateFilePathFn
	path := filepath.Join(tmpDir, "daemon.state.json")
	stateFilePathFn = func() string { return path }
	defer func() { stateFilePathFn = origFn }()

	os.WriteFile(path, []byte("not valid json{{{"), 0o644)

	state := ReadState()
	if state != nil {
		t.Errorf("ReadState() should return nil for corrupted JSON, got %+v", state)
	}
}
