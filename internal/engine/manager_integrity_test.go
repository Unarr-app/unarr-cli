package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torrentclaw/unarr/internal/agent"
)

// truncatingMockDownloader writes a SHORT file (failing the on-disk verify) until
// goodOnAttempt, then writes a full file. reportedSize is what each Result claims,
// so verify() compares the advertised size against the (initially truncated) bytes
// on disk — the exact shape of the 2026-06-15 debrid NFS truncation.
type truncatingMockDownloader struct {
	dir           string
	reportedSize  int64
	goodOnAttempt int // 1-based attempt that finally writes a full file; 0 = never
	callCount     atomic.Int32
}

func (m *truncatingMockDownloader) Method() DownloadMethod { return MethodTorrent }
func (m *truncatingMockDownloader) Available(_ context.Context, _ *Task) (bool, error) {
	return true, nil
}
func (m *truncatingMockDownloader) Download(_ context.Context, _ *Task, _ string, _ chan<- Progress) (*Result, error) {
	n := int(m.callCount.Add(1))
	path := filepath.Join(m.dir, "movie.mkv")
	size := int64(10) // truncated stub
	if m.goodOnAttempt > 0 && n >= m.goodOnAttempt {
		size = m.reportedSize
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		return nil, err
	}
	return &Result{FilePath: path, FileName: "movie.mkv", Method: MethodTorrent, Size: m.reportedSize}, nil
}
func (m *truncatingMockDownloader) Pause(_ string) error             { return nil }
func (m *truncatingMockDownloader) Cancel(_ string) error            { return nil }
func (m *truncatingMockDownloader) Shutdown(_ context.Context) error { return nil }

// captureReporter builds a ProgressReporter over a mockStatusReporter we keep a
// handle to, so the test can read the final reported StatusUpdate.
func captureReporter() (*ProgressReporter, *mockStatusReporter) {
	reporter := &mockStatusReporter{}
	return &ProgressReporter{
		reporter:     reporter,
		interval:     50 * time.Millisecond,
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
	}, reporter
}

func terminalUpdate(t *testing.T, r *mockStatusReporter, taskID string) agent.StatusUpdate {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.calls) - 1; i >= 0; i-- {
		c := r.calls[i]
		if c.TaskID == taskID && (c.Status == "completed" || c.Status == "failed") {
			return c
		}
	}
	t.Fatalf("no terminal (completed/failed) status update for %s", taskID)
	return agent.StatusUpdate{}
}

// A truncated download is re-tried clean and, once it lands intact, completes —
// "completed" is never reported for the corrupt attempt.
func TestManagerPipeline_IntegrityRetry_ThenSucceeds(t *testing.T) {
	dir := t.TempDir()
	pr, reporter := captureReporter()
	dl := &truncatingMockDownloader{dir: dir, reportedSize: 10000, goodOnAttempt: 2}

	mgr := NewManager(ManagerConfig{MaxConcurrent: 1, OutputDir: dir}, pr, dl)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go pr.Run(ctx)

	const taskID = "integrity-retry-ok-123456"
	mgr.Submit(ctx, agent.Task{
		ID: taskID, InfoHash: "abc123def456abc123def456abc123def456abc1",
		Title: "Retry Test", PreferredMethod: "torrent",
	})
	mgr.Wait()

	if got := dl.callCount.Load(); got != 2 {
		t.Errorf("download attempts = %d, want 2 (1 truncated + 1 clean)", got)
	}
	if u := terminalUpdate(t, reporter, taskID); u.Status != "completed" {
		t.Errorf("final status = %q (%s), want completed", u.Status, u.ErrorMessage)
	}
}

// A persistently-truncated download exhausts the bounded retries and is surfaced
// as damaged (failed + the stable corrupt-download marker), never completed.
func TestManagerPipeline_IntegrityRetry_ExhaustsThenDamaged(t *testing.T) {
	dir := t.TempDir()
	pr, reporter := captureReporter()
	dl := &truncatingMockDownloader{dir: dir, reportedSize: 10000, goodOnAttempt: 0}

	mgr := NewManager(ManagerConfig{MaxConcurrent: 1, OutputDir: dir}, pr, dl)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go pr.Run(ctx)

	const taskID = "integrity-retry-bad-123456"
	mgr.Submit(ctx, agent.Task{
		ID: taskID, InfoHash: "abc123def456abc123def456abc123def456abc1",
		Title: "Damaged Test", PreferredMethod: "torrent",
	})
	mgr.Wait()

	if got := dl.callCount.Load(); got != 3 {
		t.Errorf("download attempts = %d, want 3 (bounded retries)", got)
	}
	u := terminalUpdate(t, reporter, taskID)
	if u.Status != "failed" {
		t.Fatalf("final status = %q, want failed", u.Status)
	}
	if !strings.HasPrefix(u.ErrorMessage, damagedErrorPrefix) {
		t.Errorf("error message = %q, want prefix %q", u.ErrorMessage, damagedErrorPrefix)
	}
}
