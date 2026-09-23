package engine

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// gatedMockDownloader holds every Download until it receives a token on
// release (or its context ends) — the shape of a torrent stuck "waiting for
// metadata". It records start order and peak concurrency.
type gatedMockDownloader struct {
	dir     string
	release chan struct{}
	running atomic.Int32
	peak    atomic.Int32
	mu      sync.Mutex
	started []string
}

func newGatedDownloader(dir string) *gatedMockDownloader {
	return &gatedMockDownloader{dir: dir, release: make(chan struct{}, 1024)}
}

func (m *gatedMockDownloader) Method() DownloadMethod { return MethodTorrent }
func (m *gatedMockDownloader) Available(_ context.Context, _ *Task) (bool, error) {
	return true, nil
}

func (m *gatedMockDownloader) Download(ctx context.Context, t *Task, _ string, _ chan<- Progress) (*Result, error) {
	m.mu.Lock()
	m.started = append(m.started, t.ID)
	m.mu.Unlock()
	n := m.running.Add(1)
	defer m.running.Add(-1)
	for p := m.peak.Load(); n > p && !m.peak.CompareAndSwap(p, n); p = m.peak.Load() {
	}
	select {
	case <-m.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	name := t.ID + ".mkv"
	path := filepath.Join(m.dir, name)
	if err := os.WriteFile(path, make([]byte, minPlausibleVideoBytes+1), 0o644); err != nil {
		return nil, err
	}
	return &Result{FilePath: path, FileName: name, Method: MethodTorrent, Size: minPlausibleVideoBytes + 1}, nil
}
func (m *gatedMockDownloader) Pause(_ string) error             { return nil }
func (m *gatedMockDownloader) Cancel(_ string) error            { return nil }
func (m *gatedMockDownloader) Shutdown(_ context.Context) error { return nil }

func (m *gatedMockDownloader) startOrder() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.started...)
}

type queueHarness struct {
	mgr   *Manager
	dl    *gatedMockDownloader
	store *fakePersister
	ctx   context.Context
}

func newQueueHarness(t *testing.T, slots int) *queueHarness {
	t.Helper()
	dir := t.TempDir()
	pr, _ := captureReporter()
	dl := newGatedDownloader(dir)
	mgr := NewManager(ManagerConfig{MaxConcurrent: slots, OutputDir: dir}, pr, dl)
	store := newFakePersister()
	mgr.SetTaskStore(store)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	go pr.Run(ctx)
	return &queueHarness{mgr: mgr, dl: dl, store: store, ctx: ctx}
}

func queueTaskID(i int) string { return fmt.Sprintf("queue-task-%03d-0000-0000-000000000000", i) }

func (h *queueHarness) submit(i int) {
	h.mgr.Submit(h.ctx, agent.Task{
		ID: queueTaskID(i), InfoHash: fmt.Sprintf("%040d", i), Title: "Queued", PreferredMethod: "torrent",
	})
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *queueHarness) isActive(i int) bool { return h.mgr.GetTask(queueTaskID(i)) != nil }

// A full download queue must never block the caller of Submit: the startup
// resume loop and the sync loop both call it, and a blocked caller took the
// whole agent offline (2026-09-23). MaxConcurrent must still be honoured.
func TestSubmitNeverBlocksCallerOnFullQueue(t *testing.T) {
	h := newQueueHarness(t, 1)
	returned := make(chan struct{})
	go func() {
		for i := range 7 {
			h.submit(i)
		}
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit blocked its caller while the only download slot was taken")
	}
	for range 7 {
		h.dl.release <- struct{}{}
	}
	h.mgr.Wait()
	if p := h.dl.peak.Load(); p != 1 {
		t.Errorf("peak concurrent downloads = %d, want 1 (MaxConcurrent)", p)
	}
}

// Tasks start in submission order, as they did when the caller waited serially.
func TestQueuedTasksStartInSubmissionOrder(t *testing.T) {
	h := newQueueHarness(t, 1)
	const n = 12
	for i := range n {
		h.submit(i)
	}
	for i := range n {
		waitUntil(t, fmt.Sprintf("task %d to start", i), func() bool { return len(h.dl.startOrder()) == i+1 })
		h.dl.release <- struct{}{}
	}
	h.mgr.Wait()
	got := h.dl.startOrder()
	for i := range n {
		if got[i] != queueTaskID(i) {
			t.Fatalf("start order = %v, want submission order", got)
		}
	}
}

// Cancelling a queued task takes it out of the line NOW — with every slot held
// by a stuck download it would otherwise sit there forever — and the tasks
// behind it keep their place.
func TestCancelQueuedTaskLeavesLineImmediatelyAndKeepsOrder(t *testing.T) {
	h := newQueueHarness(t, 1)
	for i := range 4 { // 0 runs (stuck), 1..3 queue
		h.submit(i)
	}
	waitUntil(t, "task 0 running", func() bool { return len(h.dl.startOrder()) == 1 })

	if !h.mgr.CancelTask(queueTaskID(2)) {
		t.Fatal("CancelTask(queued) = false, want true")
	}
	waitUntil(t, "cancelled queued task to leave m.active", func() bool { return !h.isActive(2) })
	if h.store.has(queueTaskID(2)) {
		t.Error("a user cancel must drop the resume entry")
	}

	for range 3 {
		h.dl.release <- struct{}{}
	}
	h.mgr.Wait()
	want := []string{queueTaskID(0), queueTaskID(1), queueTaskID(3)}
	if got := h.dl.startOrder(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("start order = %v, want %v (cancelled 2 skipped, order kept)", got, want)
	}
}

// Pausing a queued task leaves the line but KEEPS the resume entry, like a
// pause of a running download.
func TestPauseQueuedTaskKeepsResumeEntry(t *testing.T) {
	h := newQueueHarness(t, 1)
	h.submit(0)
	h.submit(1)
	waitUntil(t, "task 0 running", func() bool { return len(h.dl.startOrder()) == 1 })

	if !h.mgr.PauseTask(queueTaskID(1)) {
		t.Fatal("PauseTask(queued) = false, want true")
	}
	waitUntil(t, "paused queued task to leave m.active", func() bool { return !h.isActive(1) })
	if !h.store.has(queueTaskID(1)) {
		t.Error("a pause must keep the resume entry")
	}
	h.dl.release <- struct{}{}
	h.mgr.Wait()
	if got := h.dl.startOrder(); len(got) != 1 {
		t.Errorf("paused task must not start, got %v", got)
	}
}

// A pause — of a running or a queued task — flags the resume entry so a restart
// keeps it paused; submitting it again (a resume) runs it and clears the flag.
func TestPauseFlagsResumeEntryAndResubmitClearsIt(t *testing.T) {
	h := newQueueHarness(t, 1)
	h.submit(0)
	h.submit(1)
	waitUntil(t, "task 0 running", func() bool { return len(h.dl.startOrder()) == 1 })

	h.mgr.PauseTask(queueTaskID(0)) // running
	h.mgr.PauseTask(queueTaskID(1)) // queued (or just dispatched)
	for i := range 2 {
		if !h.store.has(queueTaskID(i)) || !h.store.isPaused(queueTaskID(i)) {
			t.Errorf("task %d: resume entry must be kept AND flagged paused", i)
		}
	}
	h.mgr.Wait()

	h.mgr.Submit(h.ctx, agent.Task{ID: queueTaskID(1), InfoHash: fmt.Sprintf("%040d", 1), Title: "Q", PreferredMethod: "torrent", ResumePaused: true})
	if h.store.isPaused(queueTaskID(1)) {
		t.Error("a resume (Submit of the paused payload) must clear the persisted pause")
	}
	h.dl.release <- struct{}{}
	h.mgr.Wait()
}

// Shutdown with a queue full of waiters must return promptly and keep every
// resume entry: before, queued waiters ignored the task contexts Shutdown
// cancels, so Shutdown would sit out its whole timeout.
func TestShutdownWithQueuedTasksIsPromptAndKeepsResumeEntries(t *testing.T) {
	h := newQueueHarness(t, 2)
	for i := range 6 {
		h.submit(i)
	}
	waitUntil(t, "two running", func() bool { return len(h.dl.startOrder()) == 2 })

	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	h.mgr.Shutdown(sctx)
	if d := time.Since(start); d > time.Second {
		t.Errorf("Shutdown took %s with queued tasks, want prompt", d)
	}
	for i := range 6 {
		if !h.store.has(queueTaskID(i)) {
			t.Errorf("task %d lost its resume entry on shutdown", i)
		}
	}
	h.mgr.Wait()
	if got := h.dl.startOrder(); len(got) != 2 {
		t.Errorf("queued tasks started a download during shutdown: %v", got)
	}
}

// FreeSlots counts queued tasks as taken, so the sync loop never claims work
// for a slot a queued task is about to take.
func TestFreeSlotsCountsQueuedTasks(t *testing.T) {
	h := newQueueHarness(t, 2)
	if got := h.mgr.FreeSlots(); got != 2 {
		t.Fatalf("idle FreeSlots = %d, want 2", got)
	}
	for i := range 5 {
		h.submit(i)
	}
	waitUntil(t, "two running", func() bool { return len(h.dl.startOrder()) == 2 })
	if got, cap := h.mgr.FreeSlots(), h.mgr.HasCapacity(); got != 0 || cap {
		t.Errorf("FreeSlots = %d HasCapacity = %v with 2 running + 3 queued, want 0/false", got, cap)
	}
	for range 5 {
		h.dl.release <- struct{}{}
	}
	h.mgr.Wait()
	if got := h.mgr.FreeSlots(); got != 2 {
		t.Errorf("drained FreeSlots = %d, want 2", got)
	}
}

// Retry = CancelTask + an immediate Submit of the same ID (daemon_task_control).
// The cancel must take the queued task out synchronously, or Submit's dedup
// swallows the retry while the resume entry is already gone.
func TestRetryRightAfterCancellingQueuedTaskIsNotSwallowed(t *testing.T) {
	h := newQueueHarness(t, 1)
	h.submit(0)
	h.submit(1)
	waitUntil(t, "task 0 running", func() bool { return len(h.dl.startOrder()) == 1 })

	h.mgr.CancelTask(queueTaskID(1))
	h.submit(1) // the retry, with no pause in between
	if !h.isActive(1) {
		t.Fatal("retry of a just-cancelled queued task was dropped as a duplicate")
	}
	if !h.store.has(queueTaskID(1)) {
		t.Error("the retried task must be persisted again")
	}
	for range 2 {
		h.dl.release <- struct{}{}
	}
	h.mgr.Wait()
	want := []string{queueTaskID(0), queueTaskID(1)}
	if got := h.dl.startOrder(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("start order = %v, want %v", got, want)
	}
}

// The sync loop still runs while Shutdown drains; a task claimed then must not
// start with a live context Shutdown already walked past.
func TestSubmitDuringShutdownIsIgnored(t *testing.T) {
	h := newQueueHarness(t, 1)
	h.submit(0)
	waitUntil(t, "task 0 running", func() bool { return len(h.dl.startOrder()) == 1 })
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.mgr.Shutdown(sctx)

	h.submit(1)
	if h.isActive(1) || h.store.has(queueTaskID(1)) {
		t.Error("a submit after Shutdown started must be ignored, not queued or persisted")
	}
	h.mgr.Wait()
	if got := h.dl.startOrder(); len(got) != 1 {
		t.Errorf("starts = %v, want only task 0", got)
	}
}

// The daemon context can end without Shutdown (daemon.go's errCh path). Queued
// tasks must then never start and must KEEP their resume entries — they did
// not fail, the daemon went away.
func TestDaemonContextEndKeepsQueuedResumeEntries(t *testing.T) {
	h := newQueueHarness(t, 1)
	dctx, dcancel := context.WithCancel(h.ctx)
	for i := range 3 {
		h.mgr.Submit(dctx, agent.Task{ID: queueTaskID(i), InfoHash: fmt.Sprintf("%040d", i), Title: "Q", PreferredMethod: "torrent"})
	}
	waitUntil(t, "task 0 running", func() bool { return len(h.dl.startOrder()) == 1 })
	dcancel()
	h.mgr.Wait()

	if got := h.dl.startOrder(); len(got) != 1 {
		t.Errorf("queued tasks started after the daemon context ended: %v", got)
	}
	for i := 1; i < 3; i++ {
		if !h.store.has(queueTaskID(i)) {
			t.Errorf("queued task %d lost its resume entry when the daemon context ended", i)
		}
		if h.isActive(i) {
			t.Errorf("queued task %d still active after its context ended", i)
		}
	}
}

// The resume loop and a web re-dispatch deliver the same ID twice; a queued
// duplicate must not become a second download.
func TestDuplicateSubmitWhileQueuedRunsOnce(t *testing.T) {
	h := newQueueHarness(t, 1)
	h.submit(0)
	h.submit(1)
	h.submit(1)
	for range 2 {
		h.dl.release <- struct{}{}
	}
	h.mgr.Wait()
	if got := h.dl.startOrder(); len(got) != 2 {
		t.Errorf("starts = %v, want exactly 0 and 1 once each", got)
	}
}

// Force start still bypasses a full queue.
func TestForceStartBypassesFullQueue(t *testing.T) {
	h := newQueueHarness(t, 1)
	h.submit(0)
	h.submit(1)
	waitUntil(t, "task 0 running", func() bool { return len(h.dl.startOrder()) == 1 })
	h.mgr.Submit(h.ctx, agent.Task{ID: queueTaskID(9), InfoHash: fmt.Sprintf("%040d", 9), Title: "Force", PreferredMethod: "torrent", ForceStart: true})
	waitUntil(t, "force-started task running", func() bool { return len(h.dl.startOrder()) == 2 })
	if got := h.dl.startOrder()[1]; got != queueTaskID(9) {
		t.Errorf("second start = %s, want the force-started task ahead of queued task 1", got)
	}
	for range 3 {
		h.dl.release <- struct{}{}
	}
	h.mgr.Wait()
}

// Stress: concurrent submitters, random cancels and pauses, releases in
// between. Never over the slot cap, every goroutine exits, no slot or queue
// count leaks. Meaningful under -race.
func TestQueueStressNoLeaksNoOvercommit(t *testing.T) {
	const slots, n = 3, 120
	h := newQueueHarness(t, slots)
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := w; i < n; i += 4 {
				h.submit(i)
			}
		})
	}
	wg.Go(func() {
		r := rand.New(rand.NewPCG(1, 2))
		for range n / 2 {
			id := queueTaskID(r.IntN(n))
			if r.IntN(2) == 0 {
				h.mgr.CancelTask(id)
			} else {
				h.mgr.PauseTask(id)
			}
			time.Sleep(time.Duration(r.IntN(300)) * time.Microsecond)
		}
	})
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case h.dl.release <- struct{}{}:
				time.Sleep(200 * time.Microsecond)
			}
		}
	}()
	wg.Wait()

	done := make(chan struct{})
	go func() { h.mgr.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("manager goroutines never drained")
	}
	close(stop)
	if p := h.dl.peak.Load(); p > slots {
		t.Errorf("peak concurrent downloads = %d, cap %d", p, slots)
	}
	h.mgr.queueMu.Lock()
	running, queued := h.mgr.running, len(h.mgr.queue)
	h.mgr.queueMu.Unlock()
	if running != 0 || queued != 0 {
		t.Errorf("slot accounting leaked: running=%d queued=%d", running, queued)
	}
	if got := h.mgr.FreeSlots(); got != slots {
		t.Errorf("drained FreeSlots = %d, want %d", got, slots)
	}
	if a := h.mgr.ActiveCount(); a != 0 {
		t.Errorf("m.active leaked %d task(s)", a)
	}
}
