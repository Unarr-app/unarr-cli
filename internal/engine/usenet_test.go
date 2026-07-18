package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/usenet/download"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
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

func TestUsenetDownloader_MethodAndAvailable(t *testing.T) {
	u := NewUsenetDownloader(agent.NewClient("http://localhost", "", "test"))
	if got := u.Method(); got != MethodUsenet {
		t.Errorf("Method = %v, want %v", got, MethodUsenet)
	}

	// Disabled → never available, no error.
	u.SetEnabled(false)
	ok, err := u.Available(context.Background(), &Task{Title: "Foo"})
	if err != nil || ok {
		t.Errorf("disabled Available = (%v,%v), want (false,nil)", ok, err)
	}

	u.SetEnabled(true)
	// No IMDb / no title → not available, no error.
	ok, err = u.Available(context.Background(), &Task{})
	if err != nil || ok {
		t.Errorf("empty task Available = (%v,%v), want (false,nil)", ok, err)
	}

	// Pre-resolved NzbID → available immediately.
	ok, err = u.Available(context.Background(), &Task{NzbID: "preresolved", Title: "Bar"})
	if err != nil || !ok {
		t.Errorf("preresolved NzbID Available = (%v,%v), want (true,nil)", ok, err)
	}
}

func TestUsenetDownloader_Shutdown(t *testing.T) {
	u := NewUsenetDownloader(agent.NewClient("http://localhost", "", "test"))
	// Inject a fake active download — Shutdown should cancel it and clear the map.
	_, cancel := context.WithCancel(context.Background())
	u.active["t1"] = &activeDownload{cancel: cancel}
	if err := u.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown = %v, want nil", err)
	}
	if len(u.active) != 0 {
		t.Errorf("Shutdown should clear active downloads, got %d", len(u.active))
	}
}

func TestSanitizeDir(t *testing.T) {
	cases := map[string]string{
		"":                          "usenet_download",
		"normal_name":               "normal_name",
		"path/with/slashes":         "path_with_slashes",
		`win\\bad:name*?"<>|`:       "win__bad_name______",
		"con:tains/all\\bad?chars*": "con_tains_all_bad_chars_",
	}
	for in, want := range cases {
		if got := sanitizeDir(in); got != want {
			t.Errorf("sanitizeDir(%q) = %q, want %q", in, got, want)
		}
	}

	long := strings.Repeat("a", 300)
	if got := sanitizeDir(long); len(got) != 200 {
		t.Errorf("expected sanitizeDir to truncate to 200, got %d", len(got))
	}
}
