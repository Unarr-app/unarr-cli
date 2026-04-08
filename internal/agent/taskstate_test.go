package agent

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLocalState_UpdateAndSnapshot(t *testing.T) {
	s := NewLocalState()

	s.Update(TaskState{TaskID: "t1", Status: "downloading", Progress: 50})
	s.Update(TaskState{TaskID: "t2", Status: "completed", Progress: 100})

	snap := s.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(snap))
	}

	byID := make(map[string]TaskState, len(snap))
	for _, ts := range snap {
		byID[ts.TaskID] = ts
	}

	if byID["t1"].Progress != 50 {
		t.Errorf("expected progress 50, got %d", byID["t1"].Progress)
	}
	if byID["t2"].Status != "completed" {
		t.Errorf("expected completed, got %s", byID["t2"].Status)
	}
}

func TestLocalState_UpdateOverwrites(t *testing.T) {
	s := NewLocalState()

	s.Update(TaskState{TaskID: "t1", Status: "downloading", Progress: 30})
	s.Update(TaskState{TaskID: "t1", Status: "downloading", Progress: 70})

	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 task, got %d", len(snap))
	}
	if snap[0].Progress != 70 {
		t.Errorf("expected progress 70, got %d", snap[0].Progress)
	}
}

func TestLocalState_Remove(t *testing.T) {
	s := NewLocalState()

	s.Update(TaskState{TaskID: "t1", Status: "downloading"})
	s.Update(TaskState{TaskID: "t2", Status: "downloading"})
	s.Remove("t1")

	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 task, got %d", len(snap))
	}
	if snap[0].TaskID != "t2" {
		t.Errorf("expected t2, got %s", snap[0].TaskID)
	}
}

func TestLocalState_RemoveNonExistent(t *testing.T) {
	s := NewLocalState()
	s.Remove("nonexistent") // should not panic
}

func TestLocalState_SnapshotIsACopy(t *testing.T) {
	s := NewLocalState()
	s.Update(TaskState{TaskID: "t1", Status: "downloading", Progress: 50})

	snap := s.Snapshot()
	snap[0].Progress = 999

	snap2 := s.Snapshot()
	if snap2[0].Progress != 50 {
		t.Errorf("snapshot mutation leaked: got progress %d", snap2[0].Progress)
	}
}

func TestLocalState_UpdateSetsTimestamp(t *testing.T) {
	s := NewLocalState()
	s.Update(TaskState{TaskID: "t1", Status: "downloading"})

	snap := s.Snapshot()
	if snap[0].UpdatedAt == 0 {
		t.Error("expected non-zero UpdatedAt")
	}
}

func TestLocalState_ConcurrentAccess(t *testing.T) {
	s := NewLocalState()
	var wg sync.WaitGroup

	for i := range 100 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			taskID := "t" + string(rune('0'+n%10))
			s.Update(TaskState{TaskID: taskID, Status: "downloading", Progress: n})
			s.Snapshot()
			if n%3 == 0 {
				s.Remove(taskID)
			}
		}(i)
	}

	wg.Wait()
	// No race condition = test passes
}

func TestLocalState_WriteToDisk_ReadFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")

	// Override the file path for testing
	orig := taskStateFilePathFn
	taskStateFilePathFn = func() string { return path }
	defer func() { taskStateFilePathFn = orig }()

	s := NewLocalState()
	s.Update(TaskState{TaskID: "t1", Status: "downloading", Progress: 45})
	s.Update(TaskState{TaskID: "t2", Status: "completed", Progress: 100, FilePath: "/tmp/movie.mkv"})
	s.WriteToDisk()

	// Verify file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("tasks.json was not created")
	}

	// Read into a new LocalState
	s2 := NewLocalState()
	s2.ReadFromDisk()

	snap := s2.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 tasks after read, got %d", len(snap))
	}

	byID := make(map[string]TaskState, len(snap))
	for _, ts := range snap {
		byID[ts.TaskID] = ts
	}

	if byID["t1"].Progress != 45 {
		t.Errorf("expected progress 45, got %d", byID["t1"].Progress)
	}
	if byID["t2"].FilePath != "/tmp/movie.mkv" {
		t.Errorf("expected /tmp/movie.mkv, got %s", byID["t2"].FilePath)
	}
}

func TestLocalState_ReadFromDisk_CorruptedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")

	orig := taskStateFilePathFn
	taskStateFilePathFn = func() string { return path }
	defer func() { taskStateFilePathFn = orig }()

	// Write corrupted JSON
	os.WriteFile(path, []byte("{invalid json"), 0o644)

	s := NewLocalState()
	s.ReadFromDisk() // should not panic

	snap := s.Snapshot()
	if len(snap) != 0 {
		t.Errorf("expected 0 tasks from corrupted file, got %d", len(snap))
	}
}

func TestLocalState_ReadFromDisk_FileNotFound(t *testing.T) {
	orig := taskStateFilePathFn
	taskStateFilePathFn = func() string { return "/nonexistent/path/tasks.json" }
	defer func() { taskStateFilePathFn = orig }()

	s := NewLocalState()
	s.ReadFromDisk() // should not panic

	snap := s.Snapshot()
	if len(snap) != 0 {
		t.Errorf("expected 0 tasks, got %d", len(snap))
	}
}

func TestLocalState_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")

	orig := taskStateFilePathFn
	taskStateFilePathFn = func() string { return path }
	defer func() { taskStateFilePathFn = orig }()

	s := NewLocalState()
	s.Update(TaskState{TaskID: "t1", Status: "downloading"})
	s.WriteToDisk()

	// Verify no .tmp file remains
	tmpPath := path + ".tmp"
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Error("temp file should not exist after write")
	}
}

func TestLocalState_EmptySnapshot(t *testing.T) {
	s := NewLocalState()
	snap := s.Snapshot()
	if snap == nil {
		t.Error("snapshot should be non-nil empty slice")
	}
	if len(snap) != 0 {
		t.Errorf("expected 0 tasks, got %d", len(snap))
	}
}
