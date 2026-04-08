package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTestSyncClient(url string) (*SyncClient, *Client) {
	client := NewClient(url, "test-key", "test-agent/1.0")
	cfg := DaemonConfig{
		AgentID:     "test-agent",
		AgentName:   "Test",
		Version:     "1.0.0",
		DownloadDir: "/tmp/downloads",
	}
	state := NewLocalState()
	sc := NewSyncClient(client, cfg, state)
	return sc, client
}

func TestSyncClient_NewDefaults(t *testing.T) {
	sc, _ := newTestSyncClient("http://localhost")

	if sc.Watching() {
		t.Error("should not be watching initially")
	}
	if sc.currentInterval() != SyncIntervalIdle {
		t.Errorf("expected idle interval %v, got %v", SyncIntervalIdle, sc.currentInterval())
	}
}

func TestSyncClient_AdjustInterval_Watching(t *testing.T) {
	sc, _ := newTestSyncClient("http://localhost")

	sc.adjustInterval(true)

	if sc.currentInterval() != SyncIntervalWatching {
		t.Errorf("expected watching interval %v, got %v", SyncIntervalWatching, sc.currentInterval())
	}
	if !sc.Watching() {
		t.Error("expected watching=true")
	}
}

func TestSyncClient_AdjustInterval_NotWatching(t *testing.T) {
	sc, _ := newTestSyncClient("http://localhost")

	// First set watching, then unset
	sc.adjustInterval(true)
	sc.adjustInterval(false)

	if sc.currentInterval() != SyncIntervalIdle {
		t.Errorf("expected idle interval %v, got %v", SyncIntervalIdle, sc.currentInterval())
	}
	if sc.Watching() {
		t.Error("expected watching=false")
	}
}

func TestSyncClient_AdjustInterval_CallsOnWatchingChange(t *testing.T) {
	sc, _ := newTestSyncClient("http://localhost")

	var changes []bool
	sc.OnWatchingChange = func(w bool) { changes = append(changes, w) }

	sc.adjustInterval(true)
	sc.adjustInterval(true)  // no change
	sc.adjustInterval(false) // change

	if len(changes) != 2 {
		t.Fatalf("expected 2 changes, got %d: %v", len(changes), changes)
	}
	if !changes[0] {
		t.Error("first change should be true")
	}
	if changes[1] {
		t.Error("second change should be false")
	}
}

func TestSyncClient_TriggerSync_NonBlocking(t *testing.T) {
	sc, _ := newTestSyncClient("http://localhost")

	// Fill the channel
	sc.TriggerSync()
	// Should not block
	sc.TriggerSync()
	sc.TriggerSync()

	// Drain
	select {
	case <-sc.SyncNow:
	default:
		t.Error("expected a sync trigger in channel")
	}
}

func TestSyncClient_ProcessResponse_NewTasks(t *testing.T) {
	sc, _ := newTestSyncClient("http://localhost")

	var received []Task
	sc.OnNewTasks = func(tasks []Task) { received = tasks }

	sc.processResponse(&SyncResponse{
		NewTasks: []Task{
			{ID: "t1", Title: "Movie 1", InfoHash: "abc"},
			{ID: "t2", Title: "Movie 2", InfoHash: "def"},
		},
	})

	if len(received) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(received))
	}
	if received[0].Title != "Movie 1" {
		t.Errorf("expected Movie 1, got %s", received[0].Title)
	}
}

func TestSyncClient_ProcessResponse_NoTasks(t *testing.T) {
	sc, _ := newTestSyncClient("http://localhost")

	var called bool
	sc.OnNewTasks = func(tasks []Task) { called = true }

	sc.processResponse(&SyncResponse{NewTasks: nil})

	if called {
		t.Error("OnNewTasks should not be called with empty tasks")
	}
}

func TestSyncClient_ProcessResponse_Controls(t *testing.T) {
	sc, _ := newTestSyncClient("http://localhost")

	var actions []string
	var taskIDs []string
	sc.OnControl = func(action, taskID string, deleteFiles bool) {
		actions = append(actions, action)
		taskIDs = append(taskIDs, taskID)
	}

	sc.processResponse(&SyncResponse{
		Controls: []ControlAction{
			{Action: "cancel", TaskID: "task-1234-5678"},
			{Action: "pause", TaskID: "task-abcd-efgh"},
		},
	})

	if len(actions) != 2 {
		t.Fatalf("expected 2 controls, got %d", len(actions))
	}
	if actions[0] != "cancel" {
		t.Errorf("expected cancel, got %s", actions[0])
	}
	if actions[1] != "pause" {
		t.Errorf("expected pause, got %s", actions[1])
	}
}

func TestSyncClient_ProcessResponse_Upgrade(t *testing.T) {
	sc, _ := newTestSyncClient("http://localhost")

	var version string
	sc.OnUpgrade = func(v string) { version = v }

	sc.processResponse(&SyncResponse{
		Upgrade: &UpgradeSignal{Version: "2.0.0"},
	})

	if version != "2.0.0" {
		t.Errorf("expected 2.0.0, got %s", version)
	}
}

func TestSyncClient_ProcessResponse_UpgradeEmpty(t *testing.T) {
	sc, _ := newTestSyncClient("http://localhost")

	var called bool
	sc.OnUpgrade = func(v string) { called = true }

	sc.processResponse(&SyncResponse{
		Upgrade: &UpgradeSignal{Version: ""},
	})

	if called {
		t.Error("OnUpgrade should not be called with empty version")
	}
}

func TestSyncClient_ProcessResponse_Scan(t *testing.T) {
	sc, _ := newTestSyncClient("http://localhost")

	var called bool
	sc.OnScan = func() { called = true }

	sc.processResponse(&SyncResponse{Scan: true})

	if !called {
		t.Error("OnScan should have been called")
	}
}

func TestSyncClient_ProcessResponse_StreamRequests(t *testing.T) {
	sc, _ := newTestSyncClient("http://localhost")

	var received []StreamRequest
	sc.OnStreamRequest = func(sr StreamRequest) { received = append(received, sr) }

	sc.processResponse(&SyncResponse{
		StreamRequests: []StreamRequest{
			{TaskID: "t1", FilePath: "/tmp/movie.mkv"},
		},
	})

	if len(received) != 1 {
		t.Fatalf("expected 1 stream request, got %d", len(received))
	}
	if received[0].FilePath != "/tmp/movie.mkv" {
		t.Errorf("expected /tmp/movie.mkv, got %s", received[0].FilePath)
	}
}

func TestSyncClient_BuildRequest_WithGetTaskStates(t *testing.T) {
	sc, _ := newTestSyncClient("http://localhost")

	sc.GetTaskStates = func() []TaskState {
		return []TaskState{
			{TaskID: "t1", Status: "downloading", Progress: 50},
		}
	}
	sc.GetFreeSlots = func() int { return 2 }

	req := sc.buildRequest()

	if req.AgentID != "test-agent" {
		t.Errorf("expected test-agent, got %s", req.AgentID)
	}
	if len(req.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(req.Tasks))
	}
	if req.Tasks[0].Progress != 50 {
		t.Errorf("expected progress 50, got %d", req.Tasks[0].Progress)
	}
	if req.FreeSlots != 2 {
		t.Errorf("expected 2 free slots, got %d", req.FreeSlots)
	}
}

func TestSyncClient_BuildRequest_FallbackToState(t *testing.T) {
	client := NewClient("http://localhost", "key", "ua")
	state := NewLocalState()
	state.Update(TaskState{TaskID: "t1", Status: "completed", Progress: 100})

	sc := NewSyncClient(client, DaemonConfig{AgentID: "a1", Version: "1.0"}, state)
	// GetTaskStates is nil — should fall back to state.Snapshot()

	req := sc.buildRequest()
	if len(req.Tasks) != 1 {
		t.Fatalf("expected 1 task from state fallback, got %d", len(req.Tasks))
	}
}

func TestSyncClient_DoSync_Success(t *testing.T) {
	var syncCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		syncCount.Add(1)
		json.NewEncoder(w).Encode(SyncResponse{
			Watching: true,
			NewTasks: []Task{{ID: "t1", Title: "Test Movie", InfoHash: "abc"}},
		})
	}))
	defer srv.Close()

	sc, _ := newTestSyncClient(srv.URL)

	var tasksReceived []Task
	sc.OnNewTasks = func(tasks []Task) { tasksReceived = tasks }

	sc.doSync(context.Background())

	if syncCount.Load() != 1 {
		t.Errorf("expected 1 sync call, got %d", syncCount.Load())
	}
	if len(tasksReceived) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasksReceived))
	}
	if !sc.Watching() {
		t.Error("expected watching=true after sync")
	}
	if sc.currentInterval() != SyncIntervalWatching {
		t.Errorf("expected watching interval after sync")
	}
}

func TestSyncClient_DoSync_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sc, _ := newTestSyncClient(srv.URL)

	// Should not panic on error
	sc.doSync(context.Background())
}

func TestSyncClient_Run_CancelStopsLoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(SyncResponse{})
	}))
	defer srv.Close()

	sc, _ := newTestSyncClient(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := sc.Run(ctx)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestSyncClient_Run_ImmediateSyncOnTrigger(t *testing.T) {
	var syncCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		syncCount.Add(1)
		json.NewEncoder(w).Encode(SyncResponse{})
	}))
	defer srv.Close()

	sc, _ := newTestSyncClient(srv.URL)
	// Set interval to something long so only triggers cause syncs
	sc.interval.Store(int64(10 * time.Second))

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		// Wait for initial sync, then trigger 2 more
		time.Sleep(50 * time.Millisecond)
		sc.TriggerSync()
		time.Sleep(50 * time.Millisecond)
		sc.TriggerSync()
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	sc.Run(ctx)

	// Initial sync (1) + 2 triggers + final sync = 4
	count := syncCount.Load()
	if count < 3 {
		t.Errorf("expected at least 3 syncs (initial + 2 triggers), got %d", count)
	}
}
