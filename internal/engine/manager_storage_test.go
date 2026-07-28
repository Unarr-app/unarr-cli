package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// storageFailDownloader returns a StorageError (destination write failed, e.g. a
// stalled NFS mount) on every attempt until goodOnAttempt, then a clean Result.
// goodOnAttempt = 0 means it never recovers — the shape of the 2026-07-24 NFS
// fsync-timeout incident, where the BYTES were fine but the mount kept faulting.
type storageFailDownloader struct {
	dir           string
	reportedSize  int64
	goodOnAttempt int // 1-based attempt that finally persists; 0 = never
	callCount     atomic.Int32
}

func (m *storageFailDownloader) Method() DownloadMethod { return MethodDebrid }
func (m *storageFailDownloader) Available(_ context.Context, _ *Task) (bool, error) {
	return true, nil
}
func (m *storageFailDownloader) Download(_ context.Context, _ *Task, _ string, _ chan<- Progress) (*Result, error) {
	n := int(m.callCount.Add(1))
	if m.goodOnAttempt > 0 && n >= m.goodOnAttempt {
		path := writeFullFile(m.dir, m.reportedSize)
		return &Result{FilePath: path, FileName: "movie.mkv", Method: MethodDebrid, Size: m.reportedSize}, nil
	}
	// The bytes downloaded fine; persisting them to the target dir failed.
	return nil, storageErr("flush_failed", m.dir, "could not save to %s — flush to disk failed (write-back/network-mount error): input/output error", m.dir)
}
func (m *storageFailDownloader) Pause(_ string) error             { return nil }
func (m *storageFailDownloader) Cancel(_ string) error            { return nil }
func (m *storageFailDownloader) Shutdown(_ context.Context) error { return nil }

// writeFullFile writes a full-size movie.mkv into dir and returns its path.
func writeFullFile(dir string, size int64) string {
	path := filepath.Join(dir, "movie.mkv")
	_ = os.WriteFile(path, make([]byte, size), 0o644)
	return path
}

// A storage write that recovers on the second try completes — one retry is
// enough when the mount was only briefly stalled.
func TestManagerPipeline_StorageRetry_ThenSucceeds(t *testing.T) {
	dir := t.TempDir()
	pr, reporter := captureReporter()
	// Recovered file is a .mkv, so it must clear minPlausibleVideoBytes (1 MiB) or
	// verify's anti-stub floor would reject the "good" attempt.
	dl := &storageFailDownloader{dir: dir, reportedSize: minPlausibleVideoBytes + 10000, goodOnAttempt: 2}

	mgr := NewManager(ManagerConfig{MaxConcurrent: 1, OutputDir: dir}, pr, dl)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go pr.Run(ctx)

	const taskID = "storage-retry-ok-1234567"
	mgr.Submit(ctx, agent.Task{
		ID: taskID, InfoHash: "abc123def456abc123def456abc123def456abc1",
		Title: "Storage Recover Test", PreferredMethod: "debrid",
	})
	mgr.Wait()

	if got := dl.callCount.Load(); got != 2 {
		t.Errorf("download attempts = %d, want 2 (1 storage-fail + 1 recovered)", got)
	}
	if u := terminalUpdate(t, reporter, taskID); u.Status != "completed" {
		t.Errorf("final status = %q (%s), want completed", u.Status, u.ErrorMessage)
	}
}

// A persistent storage failure must NOT loop re-downloads and must NOT be
// surfaced as "corrupt": exactly ONE retry (2 attempts total, vs 3 for
// integrity), then a TERMINAL failed carrying the storage prefix.
//
// Terminal-failed, NOT a StatusCancelled "pause": a cancelled state is never
// reported to the server (ToStatusUpdate maps it to an empty apiStatus), so a
// "storage pause" would leave the task claimable and the server would re-hand it
// every sync cycle — an unstoppable re-download loop (incident 2026-07-24). A
// reported failed breaks the loop; the web shows the storage message + a Retry
// button for the user to re-run after fixing their drive/NAS.
func TestManagerPipeline_StorageFailure_TerminalFailed_NotCorrupt_NoLoop(t *testing.T) {
	dir := t.TempDir()
	p := newFakePersister()
	reporter := NewProgressReporter(agent.NewClient("http://localhost", "test", "test"), time.Hour)
	dl := &storageFailDownloader{dir: dir, reportedSize: 10000, goodOnAttempt: 0}

	mgr := NewManager(ManagerConfig{MaxConcurrent: 1, OutputDir: dir}, reporter, dl)
	mgr.SetTaskStore(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reporter.Run(ctx)

	task := agent.Task{
		ID: "storage-fail-persist-123", InfoHash: "abc123def456abc123def456abc123def456abc1",
		Title: "Storage Down Test", PreferredMethod: "debrid",
	}
	mgr.Submit(ctx, task)
	mgr.Wait()

	// ONE retry only — a broken mount doesn't heal by re-fetching gigabytes 3x.
	if got := dl.callCount.Load(); got != 2 {
		t.Errorf("download attempts = %d, want 2 (1 + a single storage retry), NOT the 3 integrity uses", got)
	}

	// A genuine terminal — the resume-store entry is dropped, so nothing re-submits
	// it in a loop. The web's Retry button re-creates it when the user is ready.
	if p.has(task.ID) {
		t.Error("storage-failed task kept in resume store — a daemon restart would re-loop it; want dropped")
	}

	// Reported as FAILED (so the server persists it and stops re-handing it),
	// with the storage prefix and NEVER the corrupt marker.
	var found bool
	for _, s := range mgr.TaskStates() {
		if s.TaskID != task.ID {
			continue
		}
		found = true
		if s.Status != "failed" {
			t.Errorf("storage-failed task reported as %q — must be failed so the server stops re-dispatching it", s.Status)
		}
		if strings.HasPrefix(s.ErrorMessage, damagedErrorPrefix) {
			t.Errorf("storage failure mislabeled as corrupt (%q) — must use the storage prefix", s.ErrorMessage)
		}
		if !strings.HasPrefix(s.ErrorMessage, storageErrorPrefix) {
			t.Errorf("error message = %q, want the storage prefix %q", s.ErrorMessage, storageErrorPrefix)
		}
	}
	if !found {
		t.Error("no final state recorded for the storage-failed task — the web would never learn it stopped")
	}
}

// A StorageError and an IntegrityError use SEPARATE retry budgets: classify()
// must not let a storage stall consume the corruption-retry allowance (they are
// different failure modes with different correct responses).
func TestStorageError_ClassificationDistinctFromIntegrity(t *testing.T) {
	se := storageErr("flush_failed", "/mnt/nas", "could not save: i/o error")
	if !IsStorage(se) {
		t.Error("IsStorage(StorageError) = false, want true")
	}
	if IsIntegrity(se) {
		t.Error("IsIntegrity(StorageError) = true — a storage failure must NOT read as integrity corruption")
	}

	ie := integrityErr("truncated", "short file")
	if !IsIntegrity(ie) {
		t.Error("IsIntegrity(IntegrityError) = false, want true")
	}
	if IsStorage(ie) {
		t.Error("IsStorage(IntegrityError) = true — corruption must NOT read as a storage failure")
	}
}

// After a storage failure, an immediate re-Submit of the same task (the sync/
// claim race that re-hands a not-yet-persisted failure) is REFUSED by the
// cooldown — it does not launch another download. This is the guard that closes
// the burst-of-re-downloads window on the agent side. A ForceStart re-submit
// bypasses it (explicit fresh user intent).
func TestSubmit_StorageCooldown_RefusesImmediateRedispatch(t *testing.T) {
	dir := t.TempDir()
	pr, _ := captureReporter()
	dl := &storageFailDownloader{dir: dir, reportedSize: 10000, goodOnAttempt: 0}

	mgr := NewManager(ManagerConfig{MaxConcurrent: 1, OutputDir: dir}, pr, dl)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go pr.Run(ctx)

	at := agent.Task{
		ID: "storage-cooldown-123", InfoHash: "abc123def456abc123def456abc123def456abc1",
		Title: "Cooldown Test", PreferredMethod: "debrid",
	}
	mgr.Submit(ctx, at)
	mgr.Wait()

	first := dl.callCount.Load()
	if first != 2 { // 1 + the single in-cycle storage retry
		t.Fatalf("first run attempts = %d, want 2", first)
	}

	// Immediate re-dispatch (server re-claimed the still-pending row): must be
	// refused — no new download attempts.
	mgr.Submit(ctx, at)
	mgr.Wait()
	if got := dl.callCount.Load(); got != first {
		t.Errorf("re-dispatch during cooldown ran the download again (attempts %d → %d); want refused", first, got)
	}

	// A ForceStart re-submit is fresh user intent — it bypasses the cooldown.
	forced := at
	forced.ForceStart = true
	mgr.Submit(ctx, forced)
	mgr.Wait()
	if got := dl.callCount.Load(); got <= first {
		t.Errorf("ForceStart re-submit was blocked by the cooldown (attempts still %d); force start must bypass it", got)
	}
}
