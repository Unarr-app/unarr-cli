package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// activeTasksFilePathFn is overridable for testing.
var activeTasksFilePathFn = func() string {
	return filepath.Join(config.DataDir(), "active-tasks.json")
}

// ActiveTaskStore persists the dispatch payloads (agent.Task) of in-flight
// DOWNLOAD tasks so the daemon can re-submit them after a restart and have the
// downloaders resume the partial data — torrent via the persisted
// piece-completion DB, debrid via HTTP Range, usenet via its segment tracker.
//
// Distinct from LocalState (in memory), which holds transient status/progress
// for syncing to the web; this holds the re-dispatch payload needed to restart
// the work. An entry is added when a download starts and removed when it
// reaches a genuine terminal state (completed / failed / cancelled) — but NOT
// when the daemon is shutting down, so an interrupted download survives the
// restart and resumes.
type ActiveTaskStore struct {
	mu    sync.Mutex
	tasks map[string]Task
}

// NewActiveTaskStore creates an empty store. Call Load() to hydrate it from disk.
func NewActiveTaskStore() *ActiveTaskStore {
	return &ActiveTaskStore{tasks: make(map[string]Task)}
}

// Add records (or replaces) a task and persists the set.
func (s *ActiveTaskStore) Add(t Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[t.ID] = t
	s.flushLocked()
}

// Remove drops a task and persists the set. No-op if absent.
func (s *ActiveTaskStore) Remove(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[taskID]; !ok {
		return
	}
	delete(s.tasks, taskID)
	s.flushLocked()
}

// Get returns the persisted dispatch payload for a task, if the store holds one.
// Used to resume a paused download without asking the server for a re-dispatch
// (the daemon may be offline, or the row may be gone entirely).
func (s *ActiveTaskStore) Get(taskID string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	return t, ok
}

// Snapshot returns every task currently held. Read by the local control server
// so `unarr downloads` can list entries that are persisted but NOT running
// (paused, or a zombie awaiting reconciliation).
func (s *ActiveTaskStore) Snapshot() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, t)
	}
	return out
}

// Clear drops every persisted task. This is the "put every zombie down" button
// behind `unarr downloads purge`; it does not touch files on disk.
func (s *ActiveTaskStore) Clear() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.tasks)
	s.tasks = make(map[string]Task)
	s.flushLocked()
	return n
}

// Load reads the persisted tasks from disk into the store and returns them.
// Returns nil on a missing or unreadable file (a fresh daemon has nothing to
// resume). Safe to call once at startup before any Add/Remove.
func (s *ActiveTaskStore) Load() []Task {
	data, err := os.ReadFile(activeTasksFilePathFn())
	if err != nil {
		return nil
	}
	var tasks []Task
	if json.Unmarshal(data, &tasks) != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = make(map[string]Task, len(tasks))
	for _, t := range tasks {
		if t.ID != "" {
			s.tasks[t.ID] = t
		}
	}
	out := make([]Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, t)
	}
	return out
}

// flushLocked atomically writes the current set to disk. Caller holds s.mu.
// Best-effort: a write failure is non-fatal (the in-memory set stays correct;
// at worst a crash before the next flush loses one resume entry).
func (s *ActiveTaskStore) flushLocked() {
	tasks := make([]Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		tasks = append(tasks, t)
	}
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return
	}
	path := activeTasksFilePathFn()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
