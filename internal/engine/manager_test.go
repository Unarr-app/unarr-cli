package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

func TestManagerSubmitAndWait(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)

	dl := &mockDownloader{method: MethodTorrent, available: true}
	mgr := NewManager(ManagerConfig{
		MaxConcurrent: 2,
		OutputDir:     t.TempDir(),
	}, reporter, dl)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go reporter.Run(ctx)

	mgr.Submit(ctx, agent.Task{
		ID:              "test-task-1",
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "Test Movie",
		PreferredMethod: "torrent",
	})

	mgr.Wait()

	// Task should have been processed (completed or failed depending on verify)
	// Since mock returns a file that doesn't exist, it may fail at verify
	// This is expected — we're testing the pipeline works
}

func TestManagerHasCapacity(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)

	mgr := NewManager(ManagerConfig{MaxConcurrent: 2}, reporter)

	if !mgr.HasCapacity() {
		t.Error("new manager should have capacity")
	}
}

func TestManagerActiveCount(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)

	mgr := NewManager(ManagerConfig{MaxConcurrent: 3}, reporter)

	if mgr.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0", mgr.ActiveCount())
	}
}

func TestManagerShutdown(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)

	dl := &mockDownloader{method: MethodTorrent, available: true}
	mgr := NewManager(ManagerConfig{
		MaxConcurrent: 1,
		OutputDir:     t.TempDir(),
	}, reporter, dl)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	mgr.Shutdown(ctx)
	// Should not hang
}

func TestManagerDefaultConcurrency(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)
	mgr := NewManager(ManagerConfig{MaxConcurrent: 0}, reporter)
	if cap(mgr.sem) != 3 {
		t.Errorf("default MaxConcurrent should be 3, got %d", cap(mgr.sem))
	}
}

func TestManagerGetTask(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)
	mgr := NewManager(ManagerConfig{MaxConcurrent: 2}, reporter)

	// No task added
	if task := mgr.GetTask("nonexistent"); task != nil {
		t.Error("expected nil for nonexistent task")
	}
}

func TestManagerActiveTasks(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)
	mgr := NewManager(ManagerConfig{MaxConcurrent: 2}, reporter)

	tasks := mgr.ActiveTasks()
	if len(tasks) != 0 {
		t.Errorf("expected 0 active tasks, got %d", len(tasks))
	}
}

func TestManagerSubmitCompletesWithValidFile(t *testing.T) {
	dir := t.TempDir()
	// Create a file that verify() will accept
	filePath := dir + "/movie.mkv"
	os.WriteFile(filePath, make([]byte, 1024), 0o644)

	reporter := &mockStatusReporter{}
	pr := &ProgressReporter{
		reporter:     reporter,
		interval:     100 * time.Millisecond,
		latest:       make(map[string]*Task),
		lastReported: make(map[string]TaskStatus),
	}

	dl := &resultMockDownloader{
		method: MethodTorrent,
		result: &Result{
			FilePath: filePath,
			FileName: "movie.mkv",
			Method:   MethodTorrent,
			Size:     1024,
		},
	}

	mgr := NewManager(ManagerConfig{
		MaxConcurrent: 2,
		OutputDir:     dir,
	}, pr, dl)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go pr.Run(ctx)

	mgr.Submit(ctx, agent.Task{
		ID:              "task-complete-test1",
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "Test Movie",
		PreferredMethod: "torrent",
	})

	mgr.Wait()
	cancel()

	// Task should have completed successfully
	// (we can't check directly since it's removed from active map after processing)
}

func TestManagerCancelTask(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)

	dl := &slowMockDownloader{method: MethodTorrent}
	mgr := NewManager(ManagerConfig{
		MaxConcurrent: 2,
		OutputDir:     t.TempDir(),
	}, reporter, dl)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go reporter.Run(ctx)

	mgr.Submit(ctx, agent.Task{
		ID:              "task-cancel-test12",
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "Cancel Me",
		PreferredMethod: "torrent",
	})

	// Give it time to start
	time.Sleep(100 * time.Millisecond)

	mgr.CancelTask("task-cancel-test12")
	mgr.Wait()
}

func TestManagerPauseTask(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)

	dl := &slowMockDownloader{method: MethodTorrent}
	mgr := NewManager(ManagerConfig{
		MaxConcurrent: 2,
		OutputDir:     t.TempDir(),
	}, reporter, dl)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go reporter.Run(ctx)

	mgr.Submit(ctx, agent.Task{
		ID:              "task-pause-test123",
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "Pause Me",
		PreferredMethod: "torrent",
	})

	time.Sleep(100 * time.Millisecond)
	mgr.PauseTask("task-pause-test123")
	mgr.Wait()
}

func TestManagerCancelAndDeleteFiles(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)

	dl := &slowMockDownloader{method: MethodTorrent}
	mgr := NewManager(ManagerConfig{
		MaxConcurrent: 2,
		OutputDir:     t.TempDir(),
	}, reporter, dl)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go reporter.Run(ctx)

	mgr.Submit(ctx, agent.Task{
		ID:              "task-delfile-test12",
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "Delete Me",
		PreferredMethod: "torrent",
	})

	time.Sleep(100 * time.Millisecond)
	mgr.CancelAndDeleteFiles("task-delfile-test12")
	mgr.Wait()
}

func TestManagerCancelNonexistent(t *testing.T) {
	reporter := NewProgressReporter(
		agent.NewClient("http://localhost", "test", "test"),
		1*time.Second,
	)
	mgr := NewManager(ManagerConfig{MaxConcurrent: 2}, reporter)
	// Should not panic
	mgr.CancelTask("nonexistent")
	mgr.PauseTask("nonexistent")
	mgr.CancelAndDeleteFiles("nonexistent")
}

// resultMockDownloader returns a configurable result
type resultMockDownloader struct {
	method DownloadMethod
	result *Result
}

func (m *resultMockDownloader) Method() DownloadMethod { return m.method }
func (m *resultMockDownloader) Available(_ context.Context, _ *Task) (bool, error) {
	return true, nil
}
func (m *resultMockDownloader) Download(_ context.Context, _ *Task, _ string, _ chan<- Progress) (*Result, error) {
	return m.result, nil
}
func (m *resultMockDownloader) Pause(_ string) error             { return nil }
func (m *resultMockDownloader) Cancel(_ string) error            { return nil }
func (m *resultMockDownloader) Shutdown(_ context.Context) error { return nil }

// slowMockDownloader blocks until context is cancelled
type slowMockDownloader struct {
	method DownloadMethod
}

func (m *slowMockDownloader) Method() DownloadMethod { return m.method }
func (m *slowMockDownloader) Available(_ context.Context, _ *Task) (bool, error) {
	return true, nil
}
func (m *slowMockDownloader) Download(ctx context.Context, _ *Task, _ string, _ chan<- Progress) (*Result, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (m *slowMockDownloader) Pause(_ string) error             { return nil }
func (m *slowMockDownloader) Cancel(_ string) error            { return nil }
func (m *slowMockDownloader) Shutdown(_ context.Context) error { return nil }

// buildPackedRelease writes a REAL split archive of payloadName into a fresh
// release dir and returns it. Stub files named .rar are useless here: extraction
// fails on them and every post-extraction branch is skipped, so a test would
// pass for the wrong reason (which is how these tests first failed).
func buildPackedRelease(t *testing.T, payloadName string) string {
	t.Helper()
	sz, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z not installed")
	}

	work := t.TempDir()
	payload := filepath.Join(work, payloadName)
	// Incompressible, so the archive really splits into volumes. A run of zeros
	// deflates below one volume and the split silently never happens.
	data := make([]byte, 2_000_000)
	seed := uint32(0x12345678)
	for i := range data {
		seed ^= seed << 13
		seed ^= seed >> 17
		seed ^= seed << 5
		data[i] = byte(seed)
	}
	if err := os.WriteFile(payload, data, 0o644); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(work, "release")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sz, "a", "-tzip", "-v1m", filepath.Join(dir, "show.zip"), payload)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not build fixture: %v\n%s", err, b)
	}
	os.Remove(payload)

	before, _ := os.ReadDir(dir)
	if len(before) < 2 {
		t.Fatalf("fixture did not split: %d file(s)", len(before))
	}
	return dir
}

// While seeding, the torrent directory must be left EXACTLY as the swarm served
// it — anacrolix keeps serving those bytes, and a private tracker's ratio
// depends on them. So the release is unpacked to a SIBLING directory: nothing is
// added to the torrent dir and nothing removed from it.
//
// The earlier design extracted in place and merely skipped its own cleanup. That
// did not protect anything: organize() moved the extracted video out and
// cleanupReleaseDir then os.RemoveAll'd the whole directory for holding no real
// video — parts included, mid-seed. See TestCleanupReleaseDir_WouldEatInPlaceParts.
func TestExtractPackedRelease_SeedingUnpacksToSiblingAndLeavesTorrentIntact(t *testing.T) {
	dir := buildPackedRelease(t, "show.mkv")
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	m := &Manager{cfg: ManagerConfig{SeedEnabled: true}}
	result := &Result{FilePath: dir, Method: MethodTorrent}
	m.extractPackedRelease(&Task{ID: "seed-test"}, result)

	// 1. The torrent dir is untouched: same entries, no extracted payload added.
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("torrent dir changed while seeding: %d entries before, %d after", len(before), len(after))
	}
	for _, e := range before {
		if _, err := os.Stat(filepath.Join(dir, e.Name())); err != nil {
			t.Errorf("archive part %s deleted while seeding: %v", e.Name(), err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "show.mkv")); err == nil {
		t.Error("extracted payload written INTO the seeding torrent dir")
	}

	// 2. The payload landed in the sibling, and the result points there so
	//    organize() finds it.
	if result.FilePath == dir {
		t.Fatal("result still points at the torrent dir: organize would find no video")
	}
	out := filepath.Join(result.FilePath, "show.mkv")
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatalf("payload not extracted to the sibling: %v", err)
	}
	if fi.Size() != 2_000_000 {
		t.Errorf("payload size = %d, want 2000000", fi.Size())
	}
}

// COUNTERFACTUAL: with seeding OFF the parts DO get cleaned up in place. Without
// this, the test above would still pass if extraction were dead code.
func TestExtractPackedRelease_CleansPartsWhenNotSeeding(t *testing.T) {
	dir := buildPackedRelease(t, "show.mkv")

	m := &Manager{cfg: ManagerConfig{SeedEnabled: false}}
	result := &Result{FilePath: dir, Method: MethodTorrent}
	m.extractPackedRelease(&Task{ID: "noseed-test"}, result)

	// Not seeding: extraction is in place, so the result must NOT be re-pointed.
	if result.FilePath != dir {
		t.Errorf("result re-pointed to %q with seeding off", result.FilePath)
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range left {
		if e.Name() != "show.mkv" {
			t.Errorf("archive part %s survived cleanup with seeding off", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "show.mkv")); err != nil {
		t.Errorf("cleanup removed the extracted video: %v", err)
	}
}

// REGRESSION for the bug the sibling design exists to prevent: extracting in
// place and keeping the parts does NOT survive organize(). cleanupReleaseDir
// removes a release dir that holds no real video — which is exactly the state an
// in-place extraction leaves behind once the video has been moved to the library.
//
// This pins the DESTRUCTIVE behaviour of the old approach, so if anyone reverts
// to extracting in place while seeding, the reason is documented and measured.
func TestCleanupReleaseDir_WouldEatInPlaceParts(t *testing.T) {
	out := t.TempDir()
	rel := filepath.Join(out, "Show.S01E01.1080p-GRP")
	if err := os.Mkdir(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	// State an in-place extraction leaves after organize moved the video out.
	for _, n := range []string{"show.part01.rar", "show.part02.rar", "show.nfo"} {
		if err := os.WriteFile(filepath.Join(rel, n), []byte("seeding payload"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cleanupReleaseDir(rel, out)

	// FAIL, not Skip: this is the load-bearing premise of the sibling design. A
	// skip here would go green in CI while the reason for the whole approach had
	// silently evaporated — exactly the invisible-regression this guards against.
	// If this ever fails, cleanupReleaseDir stopped sweeping video-less dirs and
	// the sibling indirection should be re-evaluated (not the test relaxed).
	if _, err := os.Stat(rel); !os.IsNotExist(err) {
		t.Fatal("cleanupReleaseDir no longer removes a video-less release dir: " +
			"the premise of the sibling design changed — re-evaluate it")
	}
	// Confirmed: the whole dir goes, parts included. Hence the sibling.
}

// A release dir that still holds a real video is NOT swept — the guard the test
// above depends on. Without this, TestCleanupReleaseDir_WouldEatInPlaceParts
// could be passing because cleanupReleaseDir deletes unconditionally.
func TestCleanupReleaseDir_KeepsDirWithRealVideo(t *testing.T) {
	out := t.TempDir()
	rel := filepath.Join(out, "Show.S01E02-GRP")
	if err := os.Mkdir(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, minPlausibleVideoBytes+1)
	if err := os.WriteFile(filepath.Join(rel, "show.mkv"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	cleanupReleaseDir(rel, out)

	if _, err := os.Stat(rel); err != nil {
		t.Errorf("release dir with a real video was removed: %v", err)
	}
}

// Two finalizations of the SAME release dir must not extract concurrently:
// upstream dedup keys on task ID, but the torrent dir is named after t.Name(),
// which is not unique. Two extractors over one archive set race on the output.
func TestExtractPackedRelease_SerializesPerReleaseDir(t *testing.T) {
	m := &Manager{cfg: ManagerConfig{SeedEnabled: false}}
	dir := t.TempDir()

	unlock := m.lockReleaseDir(dir)

	entered := make(chan struct{})
	go func() {
		release := m.lockReleaseDir(dir)
		close(entered)
		release()
	}()

	select {
	case <-entered:
		t.Fatal("second holder acquired the same release dir while it was locked")
	case <-time.After(50 * time.Millisecond):
	}

	unlock()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("second holder never acquired the lock after release")
	}
}

// COUNTERFACTUAL: distinct directories must NOT serialize, or every unrelated
// release would queue behind a minutes-long extraction.
func TestExtractPackedRelease_DistinctDirsDoNotBlock(t *testing.T) {
	m := &Manager{cfg: ManagerConfig{SeedEnabled: false}}

	unlock := m.lockReleaseDir(t.TempDir())
	defer unlock()

	done := make(chan struct{})
	go func() {
		release := m.lockReleaseDir(t.TempDir())
		release()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an unrelated release dir blocked on another's lock")
	}
}

// The lock table must not grow with every download the daemon ever ran.
func TestLockReleaseDir_ReleasesEntry(t *testing.T) {
	m := &Manager{cfg: ManagerConfig{}}
	dir := t.TempDir()

	unlock := m.lockReleaseDir(dir)
	unlock()

	releaseDirLocks.mu.Lock()
	n := len(releaseDirLocks.locks)
	releaseDirLocks.mu.Unlock()
	if n != 0 {
		t.Errorf("lock table retained %d entrie(s) after the last release", n)
	}
}

// REGRESSION (review finding): two tasks finalizing the SAME seeding release
// must produce ONE unpack, not two. Serializing is not enough — the second
// holder still found the source dir intact (seeding leaves it on purpose) and
// re-ran the whole extraction. Measured before the fix: two siblings, i.e. a
// 40 GB release costing 80 GB of output on top of the parts held for the swarm.
//
// Goes through extractPackedRelease with a real archive rather than exercising
// the lock in isolation: the isolated lock test passed while this bug was live.
func TestExtractPackedRelease_ConcurrentSameReleaseUnpacksOnce(t *testing.T) {
	out := t.TempDir()
	src := buildPackedRelease(t, "show.mkv")
	rel := filepath.Join(out, "Show.S01E14-GRP")
	if err := os.Rename(src, rel); err != nil {
		t.Fatal(err)
	}

	m := &Manager{cfg: ManagerConfig{SeedEnabled: true, OutputDir: out}}
	r1 := &Result{FilePath: rel, Method: MethodTorrent}
	r2 := &Result{FilePath: rel, Method: MethodTorrent}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m.extractPackedRelease(&Task{ID: "concurrent-a"}, r1) }()
	go func() { defer wg.Done(); m.extractPackedRelease(&Task{ID: "concurrent-b"}, r2) }()
	wg.Wait()

	if r1.FilePath != r2.FilePath {
		t.Errorf("payload unpacked twice: %q and %q", r1.FilePath, r2.FilePath)
	}
	// COUNTERFACTUAL on the assertion above: both must point at a REAL unpack,
	// not merely agree by both having failed and stayed on the release dir.
	if r1.FilePath == rel {
		t.Fatal("no extraction happened at all — the test proves nothing")
	}
	if _, err := os.Stat(filepath.Join(r1.FilePath, "show.mkv")); err != nil {
		t.Errorf("adopted unpack has no payload: %v", err)
	}

	// Exactly one unpack holder beside the release.
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	var unpacked int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), unpackedSuffix) || strings.Contains(e.Name(), unpackedSuffix+".") {
			unpacked++
		}
	}
	if unpacked != 1 {
		t.Errorf("want 1 unpack directory, got %d", unpacked)
	}
	// The seeding release itself is still whole.
	if _, err := os.Stat(filepath.Join(rel, "show.zip.001")); err != nil {
		t.Errorf("seeding release damaged: %v", err)
	}
}

// A half-written sibling (extraction died mid-way) must NOT be adopted: filing
// a truncated video into the library is worse than unpacking again.
func TestExistingUnpackedSibling_IgnoresIncompleteUnpack(t *testing.T) {
	out := t.TempDir()
	rel := filepath.Join(out, "Show.S01E15-GRP")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}

	// A holder whose payload is below the plausible-video floor: a partial write.
	partial := unpackedSiblingDir(rel)
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "show.mkv"), []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := existingUnpackedSibling(rel); got != "" {
		t.Errorf("adopted an incomplete unpack: %q", got)
	}

	// COUNTERFACTUAL: a complete one IS adopted, so the check above rejects for
	// the right reason instead of never matching anything.
	if err := os.WriteFile(filepath.Join(partial, "show.mkv"), make([]byte, minPlausibleVideoBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := existingUnpackedSibling(rel); got != partial {
		t.Errorf("complete unpack not adopted: got %q, want %q", got, partial)
	}
}

// REGRESSION (review finding): the ".unpacked" holder is transient — organize
// empties it and nobody owns it afterwards, so it must be reclaimed. Measured
// before the fix: 4 re-downloads left .unpacked, .2, .3 and .4 in the download
// dir, each one bumping the collision counter for the next unpack.
func TestRemoveEmptyUnpackHolder(t *testing.T) {
	// withOut builds a manager whose download dir is out — the containment
	// boundary the cleanup must respect.
	withOut := func(out string) *Manager {
		return &Manager{cfg: ManagerConfig{OutputDir: out}}
	}

	t.Run("removes our own empty holder", func(t *testing.T) {
		out := t.TempDir()
		child := unpackedSiblingDir(filepath.Join(out, "Show.S01E01-GRP"))
		if err := os.MkdirAll(child, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(child); err != nil { // organize consumed the child
			t.Fatal(err)
		}

		withOut(out).removeEmptyUnpackHolder(child)

		if _, err := os.Stat(filepath.Dir(child)); !os.IsNotExist(err) {
			t.Error("empty holder survived")
		}
	})

	// REGRESSION (review finding): the suffix check alone let this delete the
	// user's DOWNLOAD DIR. Someone whose downloads live in "~/media.unpacked"
	// lost it the first time a release organized cleanly out of it (measured).
	// The holder must be strictly inside the download dir, never the dir itself.
	t.Run("never deletes the download dir itself", func(t *testing.T) {
		home := t.TempDir()
		out := filepath.Join(home, "media"+unpackedSuffix) // the user named it
		rel := filepath.Join(out, "Movie.2024-GRP")
		if err := os.MkdirAll(rel, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(rel); err != nil {
			t.Fatal(err)
		}

		withOut(out).removeEmptyUnpackHolder(rel)

		if _, err := os.Stat(out); err != nil {
			t.Errorf("the user's download dir was deleted: %v", err)
		}
	})

	// A holder outside the download dir is not ours to remove either.
	t.Run("ignores a holder outside the download dir", func(t *testing.T) {
		home := t.TempDir()
		out := filepath.Join(home, "downloads")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		holder := filepath.Join(home, "elsewhere"+unpackedSuffix)
		if err := os.MkdirAll(holder, 0o755); err != nil {
			t.Fatal(err)
		}

		withOut(out).removeEmptyUnpackHolder(filepath.Join(holder, "Movie-GRP"))

		if _, err := os.Stat(holder); err != nil {
			t.Errorf("holder outside the download dir was deleted: %v", err)
		}
	})

	// With no download dir configured there is nothing to contain against, so
	// nothing may be deleted.
	t.Run("no-op without a configured download dir", func(t *testing.T) {
		home := t.TempDir()
		holder := filepath.Join(home, "Movie-GRP"+unpackedSuffix)
		if err := os.MkdirAll(holder, 0o755); err != nil {
			t.Fatal(err)
		}

		(&Manager{}).removeEmptyUnpackHolder(filepath.Join(holder, "Movie-GRP"))

		if _, err := os.Stat(holder); err != nil {
			t.Errorf("holder removed with no OutputDir configured: %v", err)
		}
	})

	// A holder that still carries files is NOT ours to destroy: organize's
	// no-video fallback may have left the release in there.
	t.Run("keeps a non-empty holder", func(t *testing.T) {
		out := t.TempDir()
		child := unpackedSiblingDir(filepath.Join(out, "Show.S01E02-GRP"))
		if err := os.MkdirAll(child, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(child, "show.mkv"), []byte("payload"), 0o644); err != nil {
			t.Fatal(err)
		}

		withOut(out).removeEmptyUnpackHolder(child)

		if _, err := os.Stat(child); err != nil {
			t.Errorf("holder with content was removed: %v", err)
		}
	})

	// THE guard that matters: a directory the user made must never be touched,
	// however empty it is. Without the suffix check this would delete the parent
	// of every in-place (non-seeding) release.
	t.Run("never touches a directory that is not ours", func(t *testing.T) {
		out := t.TempDir()
		rel := filepath.Join(out, "Movies", "Show.S01E03-GRP")
		if err := os.MkdirAll(rel, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(rel); err != nil {
			t.Fatal(err)
		}

		withOut(out).removeEmptyUnpackHolder(rel)

		if _, err := os.Stat(filepath.Join(out, "Movies")); err != nil {
			t.Errorf("a user directory was deleted: %v", err)
		}
	})

	t.Run("numbered holder is recognised", func(t *testing.T) {
		out := t.TempDir()
		holder := filepath.Join(out, "Show.S01E04-GRP"+unpackedSuffix+".3")
		child := filepath.Join(holder, "Show.S01E04-GRP")
		if err := os.MkdirAll(holder, 0o755); err != nil {
			t.Fatal(err)
		}

		withOut(out).removeEmptyUnpackHolder(child)

		if _, err := os.Stat(holder); !os.IsNotExist(err) {
			t.Error("numbered holder survived")
		}
	})

	t.Run("empty path is a no-op", func(t *testing.T) {
		withOut(t.TempDir()).removeEmptyUnpackHolder("")
	})
}

// A re-download must not unpack onto a previous result.
func TestUnpackedSiblingDir_AvoidsCollision(t *testing.T) {
	work := t.TempDir()
	src := filepath.Join(work, "Show.S01E01")

	first := unpackedSiblingDir(src)
	if err := os.MkdirAll(first, 0o755); err != nil {
		t.Fatal(err)
	}
	second := unpackedSiblingDir(src)

	if second == first {
		t.Errorf("collision: both unpacks target %q", first)
	}
	// The unpack holder sits beside the release, and the directory organize is
	// handed is named after the RELEASE — its no-video fallback renames that
	// directory into the library as-is, so ".unpacked" must never be the name.
	if filepath.Base(first) != filepath.Base(src) {
		t.Errorf("organize target is named %q, want the release name %q",
			filepath.Base(first), filepath.Base(src))
	}
	if filepath.Dir(filepath.Dir(first)) != work {
		t.Errorf("unpack holder %q is not beside the release", first)
	}
}

// A single-file result cannot be a packed release; it must be left alone.
func TestExtractPackedRelease_IgnoresSingleFileResult(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{cfg: ManagerConfig{SeedEnabled: false}}
	m.extractPackedRelease(&Task{ID: "single"}, &Result{FilePath: file, Method: MethodTorrent})

	if _, err := os.Stat(file); err != nil {
		t.Errorf("single-file result was touched: %v", err)
	}
}

// REGRESSION (review finding, CRITICAL): a daemon killed mid-extraction left a
// half-written video behind, and the resumed task ADOPTED it as finished — a
// 3 MiB file of a 2 GB film filed into the library, reported COMPLETED, resume
// entry dropped, no recovery. Measured.
//
// The fix makes existence mean completion: the unpack is written to a ".tmp"
// holder and renamed into place only after the extractor succeeds. A size floor
// cannot substitute for that — any real video crosses it in the first second of
// writing.
func TestExtractPackedRelease_NeverAdoptsUnfinishedUnpack(t *testing.T) {
	out := t.TempDir()
	rel := filepath.Join(out, "Show.S01E01-GRP")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}

	// What a crash during extraction leaves: a ".tmp" holder with a partial
	// video that is comfortably over the plausible-video floor.
	final := unpackedSiblingDir(rel)
	tmpHolder := filepath.Dir(final) + unpackedTmpSuffix
	partial := filepath.Join(tmpHolder, filepath.Base(final))
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "show.mkv"), make([]byte, 3<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := existingUnpackedSibling(rel); got != "" {
		t.Errorf("adopted an unfinished unpack at %q", got)
	}
}

// REGRESSION (review finding): the stale-".tmp" reap lived inside the seeding
// branch only. A daemon killed mid-extraction whose user then turned seeding
// OFF left the orphan — up to the full release size — forever: the in-place
// retry never looked for it, nothing else matches the suffix, and adoption
// (correctly) refuses it. Numbered holders (".unpacked.2.tmp") were equally
// unreachable when their base ".unpacked" later disappeared.
func TestExtractPackedRelease_ReapsStaleTmpOnNonSeedingPath(t *testing.T) {
	out := t.TempDir()
	src := buildPackedRelease(t, "show.mkv")
	rel := filepath.Join(out, "Show.S01E03-GRP")
	if err := os.Rename(src, rel); err != nil {
		t.Fatal(err)
	}

	// What a crash during a SEEDING extraction leaves behind, plus a numbered
	// variant from an earlier collision.
	for _, holder := range []string{
		rel + unpackedSuffix + unpackedTmpSuffix,
		rel + unpackedSuffix + ".2" + unpackedTmpSuffix,
	} {
		if err := os.MkdirAll(holder, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(holder, "half.mkv"), make([]byte, 2<<20), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Seeding is now OFF: the retry takes the in-place path.
	m := &Manager{cfg: ManagerConfig{SeedEnabled: false, OutputDir: out}}
	result := &Result{FilePath: rel, Method: MethodTorrent}
	m.extractPackedRelease(&Task{ID: "reap-test"}, result)

	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), unpackedTmpSuffix) {
			t.Errorf("stale %q holder survived the non-seeding retry: %s", unpackedTmpSuffix, e.Name())
		}
	}
	// COUNTERFACTUAL: the retry itself really worked — the release was unpacked
	// in place — so the reap is not passing because nothing ran at all.
	if _, err := os.Stat(filepath.Join(rel, "show.mkv")); err != nil {
		t.Errorf("in-place retry did not unpack: %v", err)
	}
}

// The other half of the guarantee: a COMPLETED unpack really is renamed into
// place, adoptable, and leaves no ".tmp" behind. Without this the test above
// would pass with adoption broken outright.
func TestExtractPackedRelease_FinishedUnpackIsRenamedIntoPlace(t *testing.T) {
	out := t.TempDir()
	src := buildPackedRelease(t, "show.mkv")
	rel := filepath.Join(out, "Show.S01E02-GRP")
	if err := os.Rename(src, rel); err != nil {
		t.Fatal(err)
	}

	m := &Manager{cfg: ManagerConfig{SeedEnabled: true, OutputDir: out}}
	result := &Result{FilePath: rel, Method: MethodTorrent}
	m.extractPackedRelease(&Task{ID: "rename-test"}, result)

	if _, err := os.Stat(filepath.Join(result.FilePath, "show.mkv")); err != nil {
		t.Fatalf("payload missing after the finalizing rename: %v", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), unpackedTmpSuffix) {
			t.Errorf("a %q holder survived a successful extraction: %s", unpackedTmpSuffix, e.Name())
		}
	}
	// And it is now adoptable, which is what makes the concurrent-task and
	// resume paths reuse it instead of unpacking again.
	if existingUnpackedSibling(rel) == "" {
		t.Error("a finished unpack is not adoptable")
	}
}
