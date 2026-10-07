package agent

import (
	"os"
	"testing"
	"time"
)

// A pause must survive a restart: the flag is written to disk and read back.
func TestActiveTaskStore_SetPausedPersists(t *testing.T) {
	withTempStorePath(t)
	s := NewActiveTaskStore()
	s.Add(Task{ID: "a", Title: "A"})
	s.SetPaused("a", true)
	s.SetPaused("ghost", true) // not persisted: must be a no-op, not an insert

	loaded := NewActiveTaskStore().Load()
	if len(loaded) != 1 || !loaded[0].ResumePaused {
		t.Fatalf("loaded = %+v, want one task flagged paused", loaded)
	}

	s.SetPaused("a", false)
	if NewActiveTaskStore().Load()[0].ResumePaused {
		t.Error("unpausing must clear the persisted flag")
	}
}

// Re-adding a task (a local resume or retry) keeps its original queue position.
func TestActiveTaskStore_AddKeepsQueuedAt(t *testing.T) {
	withTempStorePath(t)
	s := NewActiveTaskStore()
	s.Add(Task{ID: "a"})
	first, _ := s.Get("a")
	if first.QueuedAt.IsZero() {
		t.Fatal("a new entry must be stamped with QueuedAt")
	}
	time.Sleep(2 * time.Millisecond)
	s.Add(Task{ID: "a", Title: "re-added"})
	again, _ := s.Get("a")
	if !again.QueuedAt.Equal(first.QueuedAt) || again.Title != "re-added" {
		t.Errorf("re-add: QueuedAt %v -> %v, title %q", first.QueuedAt, again.QueuedAt, again.Title)
	}
}

// The boot resume order is oldest first, and stable for entries written by an
// older agent that never stamped QueuedAt.
func TestActiveTaskStore_LoadOrdersOldestFirst(t *testing.T) {
	withTempStorePath(t)
	base := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	s := NewActiveTaskStore()
	s.Add(Task{ID: "c", QueuedAt: base.Add(2 * time.Hour)})
	s.Add(Task{ID: "a", QueuedAt: base.Add(1 * time.Hour)})
	s.Add(Task{ID: "b", QueuedAt: base})

	var got []string
	for _, tk := range NewActiveTaskStore().Load() {
		got = append(got, tk.ID)
	}
	if len(got) != 3 || got[0] != "b" || got[1] != "a" || got[2] != "c" {
		t.Errorf("load order = %v, want [b a c]", got)
	}
}

// active-tasks.json written by an agent before these fields existed loads
// unchanged: nothing paused, nothing lost.
func TestActiveTaskStore_LoadsPreFlagFile(t *testing.T) {
	withTempStorePath(t)
	legacy := `[{"id":"old-2","infoHash":"h2","title":"Two","preferredMethod":"auto"},
	            {"id":"old-1","infoHash":"h1","title":"One","preferredMethod":"auto"}]`
	if err := os.WriteFile(activeTasksFilePathFn(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded := NewActiveTaskStore().Load()
	if len(loaded) != 2 || loaded[0].ID != "old-1" || loaded[1].ID != "old-2" {
		t.Fatalf("loaded = %+v, want both, ordered by ID", loaded)
	}
	for _, tk := range loaded {
		if tk.ResumePaused {
			t.Errorf("%s loaded as paused from a file without the flag", tk.ID)
		}
	}
}
