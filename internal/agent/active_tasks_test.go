package agent

import (
	"path/filepath"
	"testing"
)

// withTempStorePath points the store file at a temp location for the duration
// of a test and restores the original afterward.
func withTempStorePath(t *testing.T) {
	t.Helper()
	orig := activeTasksFilePathFn
	path := filepath.Join(t.TempDir(), "active-tasks.json")
	activeTasksFilePathFn = func() string { return path }
	t.Cleanup(func() { activeTasksFilePathFn = orig })
}

func TestActiveTaskStore_AddLoadRoundTrip(t *testing.T) {
	withTempStorePath(t)

	s := NewActiveTaskStore()
	s.Add(Task{ID: "a", InfoHash: "hashA", Title: "Movie A", Mode: "download"})
	s.Add(Task{ID: "b", NzbID: "nzbB", Title: "Show B"})

	// A fresh store hydrated from disk must see both.
	loaded := NewActiveTaskStore().Load()
	if len(loaded) != 2 {
		t.Fatalf("Load returned %d tasks, want 2", len(loaded))
	}
	byID := map[string]Task{}
	for _, tk := range loaded {
		byID[tk.ID] = tk
	}
	if byID["a"].InfoHash != "hashA" || byID["a"].Title != "Movie A" {
		t.Errorf("task a not round-tripped: %+v", byID["a"])
	}
	if byID["b"].NzbID != "nzbB" {
		t.Errorf("task b not round-tripped: %+v", byID["b"])
	}
}

func TestActiveTaskStore_Remove(t *testing.T) {
	withTempStorePath(t)

	s := NewActiveTaskStore()
	s.Add(Task{ID: "a", Title: "A"})
	s.Add(Task{ID: "b", Title: "B"})
	s.Remove("a")
	s.Remove("missing") // no-op

	loaded := NewActiveTaskStore().Load()
	if len(loaded) != 1 || loaded[0].ID != "b" {
		t.Fatalf("after Remove(a), Load = %+v, want only b", loaded)
	}
}

func TestActiveTaskStore_Overwrite(t *testing.T) {
	withTempStorePath(t)

	s := NewActiveTaskStore()
	s.Add(Task{ID: "a", Title: "old"})
	s.Add(Task{ID: "a", Title: "new"}) // same id replaces

	loaded := NewActiveTaskStore().Load()
	if len(loaded) != 1 || loaded[0].Title != "new" {
		t.Fatalf("overwrite failed: %+v", loaded)
	}
}

func TestActiveTaskStore_LoadMissingFile(t *testing.T) {
	withTempStorePath(t) // temp dir, no file written yet
	if got := NewActiveTaskStore().Load(); got != nil {
		t.Errorf("Load on missing file = %+v, want nil", got)
	}
}
