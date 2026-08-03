package engine

import (
	"strings"
	"sync"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// TestTaskConcurrentWriteVsStatusUpdate reproduces the production race between
// the goroutine that owns a download (manager / stream handler) and the
// ProgressReporter goroutine, which reads the same Task through
// ToStatusUpdate every tick. Every field ToStatusUpdate touches is written
// here through its guarded setter; run under -race, this fails the moment any
// of those setters loses its lock (or a call site goes back to assigning the
// field directly).
func TestTaskConcurrentWriteVsStatusUpdate(t *testing.T) {
	task := NewTaskFromAgent(agent.Task{
		ID:              "11111111-2222-3333-4444-555555555555",
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "The Matrix (1999)",
		PreferredMethod: "auto",
	})

	const iterations = 500
	var wg sync.WaitGroup

	// Writer: the download-owning goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			task.SetError("cancelled by user")
			task.SetFilePath("/downloads/movies/matrix.mkv")
			task.SetFileName("matrix.mkv")
			task.SetTotalBytes(int64(i) * 1024)
			task.SetStreamURL("http://127.0.0.1:8080/stream")
			task.SetResolvedMethod(MethodDebrid)
			task.UpdateProgress(Progress{
				DownloadedBytes: int64(i),
				TotalBytes:      int64(i) * 1024,
				SpeedBps:        int64(i),
				ETA:             i,
				FileName:        "matrix.mkv",
			})
		}
	}()

	// Second writer: the state machine, driven by API handlers on their own
	// goroutines (cancel/pause) — Transition writes Status and clears ErrorMessage.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			task.Transition(StatusResolving)
			task.Transition(StatusDownloading)
		}
	}()

	// Reader: the ProgressReporter flush loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			u := task.ToStatusUpdate()
			// Torn reads surface as garbage rather than as one of the two
			// values the writer ever sets.
			if u.FileName != "" && u.FileName != "matrix.mkv" {
				t.Errorf("torn FileName read: %q", u.FileName)
			}
			_ = task.Percent()
			_ = task.GetStatus()
			_ = task.GetError()
			_ = task.GetStreamURL()
			_ = task.GetResolvedMethod()
		}
	}()

	wg.Wait()
}

// TestTaskConcurrentFallbackVsStatusUpdate exercises the real fallback path
// (tryFallback appends to TriedMethods and reads ResolvedMethod) against the
// reporter's ToStatusUpdate + HasUntried, which read the same fields.
func TestTaskConcurrentFallbackVsStatusUpdate(t *testing.T) {
	downloaders := map[DownloadMethod]Downloader{
		MethodTorrent: &mockDownloader{method: MethodTorrent, available: true},
		MethodDebrid:  &mockDownloader{method: MethodDebrid, available: true},
		MethodUsenet:  &mockDownloader{method: MethodUsenet, available: true},
	}
	available := []DownloadMethod{MethodTorrent, MethodDebrid, MethodUsenet}

	task := NewTaskFromAgent(agent.Task{
		ID:              "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		PreferredMethod: "auto",
	})
	task.SetResolvedMethod(MethodTorrent)

	const iterations = 300
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			tryFallback(task, downloaders, nil)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = task.ToStatusUpdate()
			_ = task.HasUntried(available)
			_ = task.TriedMethodsSnapshot()
		}
	}()

	wg.Wait()
}

// TestTriedMethodsSnapshotIsACopy guards the snapshot contract: callers range
// over the returned slice without the lock, so it must not alias the live
// backing array that MarkTried appends to.
func TestTriedMethodsSnapshotIsACopy(t *testing.T) {
	task := &Task{}
	task.MarkTried(MethodTorrent)

	snap := task.TriedMethodsSnapshot()
	if len(snap) != 1 || snap[0] != MethodTorrent {
		t.Fatalf("snapshot = %v, want [torrent]", snap)
	}

	snap[0] = MethodUsenet
	if got := task.TriedMethodsSnapshot(); got[0] != MethodTorrent {
		t.Errorf("mutating the snapshot changed the task: %v", got)
	}
}

// TestTaskShortID covers the ID[:8] panic this replaced: the one-shot download
// path mints synthetic ids, and a short one used to crash the log line.
func TestTaskShortID(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"11111111-2222-3333-4444-555555555555", "11111111"},
		{"12345678", "12345678"},
		{"short", "short"},
		{"", ""},
	}
	for _, tt := range tests {
		task := &Task{ID: tt.id}
		if got := task.ShortID(); got != tt.want {
			t.Errorf("ShortID(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

// TestSetErrorTruncationRoundTrip checks the setter feeds the same truncation
// ToStatusUpdate applies, so an oversized unrar/par2 dump still reports.
func TestSetErrorTruncationRoundTrip(t *testing.T) {
	task := NewTaskFromAgent(agent.Task{ID: "id-1"})
	task.SetError(strings.Repeat("x", 5000))

	if got := len(task.GetError()); got != 5000 {
		t.Errorf("GetError length = %d, want 5000 (setter must not truncate)", got)
	}
	if got := len(task.ToStatusUpdate().ErrorMessage); got != 2000 {
		t.Errorf("reported ErrorMessage length = %d, want 2000", got)
	}
}
