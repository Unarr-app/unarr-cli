package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/usenet/download"
	"github.com/torrentclaw/unarr/internal/usenet/nzb"
)

// emptyNZB returns a minimal NZB with no files, suitable for test tracker creation.
func emptyNZB() *nzb.NZB { return &nzb.NZB{} }

// TestUsenetDownloader_Cancel_NoRace verifies that Cancel() reads tracker and taskDir
// under the mutex, avoiding a data race with Download() which writes them under the same lock.
// Run with -race to detect the race if it regresses.
func TestUsenetDownloader_Cancel_NoRace(t *testing.T) {
	u := NewUsenetDownloader(agent.NewClient("http://localhost", "", "test"))

	const taskID = "race-test-taskid-123456"

	// Inject a fake activeDownload without tracker/taskDir set yet.
	// We only need the cancel func; discard the context itself.
	_, cancel := context.WithCancel(context.Background())
	dl := &activeDownload{cancel: cancel}
	u.mu.Lock()
	u.active[taskID] = dl
	u.mu.Unlock()

	var wg sync.WaitGroup

	// Goroutine 1: simulates Download() setting tracker and taskDir under lock.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			tracker := download.NewProgressTracker(taskID, emptyNZB(), t.TempDir())
			u.mu.Lock()
			dl.tracker = tracker
			dl.taskDir = t.TempDir()
			u.mu.Unlock()
			time.Sleep(time.Microsecond)
		}
	}()

	// Goroutine 2: calls Cancel() concurrently — must read under lock.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			u.Cancel(taskID) //nolint:errcheck
			time.Sleep(time.Microsecond)
		}
	}()

	wg.Wait()
}

// TestUsenetDownloader_Cancel_NonExistent verifies Cancel on unknown task returns nil.
func TestUsenetDownloader_Cancel_NonExistent(t *testing.T) {
	u := NewUsenetDownloader(agent.NewClient("http://localhost", "", "test"))
	if err := u.Cancel("no-such-task"); err != nil {
		t.Errorf("Cancel non-existent task = %v, want nil", err)
	}
}

// TestUsenetDownloader_Pause_NonExistent verifies Pause on unknown task returns nil.
func TestUsenetDownloader_Pause_NonExistent(t *testing.T) {
	u := NewUsenetDownloader(agent.NewClient("http://localhost", "", "test"))
	if err := u.Pause("no-such-task"); err != nil {
		t.Errorf("Pause non-existent task = %v, want nil", err)
	}
}
