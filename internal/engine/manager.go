package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/notify"
	"github.com/Unarr-app/unarr-cli/internal/usenet/postprocess"
)

// ManagerConfig holds download manager settings.
type ManagerConfig struct {
	MaxConcurrent int
	OutputDir     string
	Organize      OrganizeConfig
	Notifications bool // send desktop notifications on complete/fail
	// PreferredMethods is the agent's ordered download-method preference from
	// config.toml (e.g. ["debrid","usenet"]). Non-empty → it gates which methods
	// resolveMethod will try, ignoring the per-task preference. Empty/nil → defer
	// to the task's web-sent preference (legacy auto/torrent-first).
	PreferredMethods []string
	// SeedEnabled mirrors TorrentConfig.SeedEnabled. The manager needs it because
	// a seeding torrent keeps serving the EXACT files it downloaded: deleting the
	// archive parts after unpacking a packed release would silently break seeding
	// (and the ratio obligation on a private tracker). Mirrored rather than reached
	// through the downloader so the post-processing decision does not depend on
	// which method happened to resolve.
	SeedEnabled bool
}

// Manager orchestrates concurrent downloads with method resolution and fallback.
type Manager struct {
	cfg         ManagerConfig
	reporter    *ProgressReporter
	downloaders map[DownloadMethod]Downloader

	activeMu sync.RWMutex
	active   map[string]*Task
	cancels  map[string]context.CancelFunc // per-task cancel functions

	sem chan struct{}
	wg  sync.WaitGroup

	// OnTaskDone is called after a task completes or fails (slot freed).
	// Used by the daemon to trigger an immediate sync.
	OnTaskDone func()

	// OnStateChange is called after EVERY successful task status transition
	// (resolving → downloading → verifying → organizing → seeding → done/failed),
	// wired by the daemon to trigger an immediate sync so the server sees state
	// changes in near-realtime instead of on the next adaptive tick. Coalesced
	// downstream (TriggerSync is a buffered-1 send), so bursts collapse safely.
	OnStateChange func()

	// recentlyFinished holds tasks that completed/failed since the last sync read.
	// The sync goroutine reads and clears this to include final states in the next sync.
	recentMu       sync.Mutex
	recentFinished []agent.TaskState

	// taskStore persists in-flight download payloads so the daemon can re-submit
	// them after a restart (the downloaders resume the partial data). nil = no
	// persistence. shuttingDown gates removal: a task interrupted by a graceful
	// shutdown keeps its store entry (so it resumes), unlike a genuine terminal.
	taskStore    taskPersister
	shuttingDown atomic.Bool

	// storageFailedAt records when a task last failed with a StorageError, so
	// Submit can refuse to re-run it during a cooldown. A storage failure is
	// terminal on the server, but between the agent reporting `failed` and the
	// server persisting it, an in-flight sync can re-claim the still-"pending"
	// row and re-dispatch it — a burst of pointless full re-downloads to the same
	// broken mount before the failed state settles (incident 2026-07-24). This
	// agent-side guard closes that window regardless of why the server re-hands
	// it: a task that just hit storage trouble is not retried until the cooldown
	// elapses (a manual Retry from the web is a fresh user intent — see the note
	// in Submit — and is NOT blocked, because the web clears the error first).
	storageFailMu   sync.Mutex
	storageFailedAt map[string]time.Time

	// pausedTasks holds the IDs of tasks paused on purpose (web control, local
	// `unarr downloads pause`, tray). A pause cancels the task context, so the
	// download goroutine unwinds through fail() → recordFinished, which would
	// DROP the resume-store entry and turn a pause into "start from zero after a
	// restart". Membership here flips recordFinished to the resume-preserving
	// variant. Cleared on cancel and on the next Submit (a resume is a fresh run).
	pausedMu    sync.Mutex
	pausedTasks map[string]struct{}
}

// storageFailCooldown is how long Submit refuses to re-run a task that just
// failed with a StorageError. Long enough to outlast the sync/claim race that
// re-dispatches a not-yet-persisted failure, short enough that a genuine manual
// retry minutes later is never affected.
const storageFailCooldown = 60 * time.Second

// taskPersister is the resume store the manager records in-flight downloads to.
// Satisfied by *agent.ActiveTaskStore; an interface so tests can inject a fake.
type taskPersister interface {
	Add(agent.Task)
	Remove(taskID string)
}

// SetTaskStore wires the resume store. Call once before Submit. Optional —
// without it, downloads are not persisted for cross-restart resume.
func (m *Manager) SetTaskStore(s taskPersister) { m.taskStore = s }

// NewManager creates a download manager.
func NewManager(cfg ManagerConfig, reporter *ProgressReporter, downloaders ...Downloader) *Manager {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 3
	}

	dlMap := make(map[DownloadMethod]Downloader)
	for _, d := range downloaders {
		dlMap[d.Method()] = d
	}

	return &Manager{
		cfg:             cfg,
		reporter:        reporter,
		downloaders:     dlMap,
		active:          make(map[string]*Task),
		cancels:         make(map[string]context.CancelFunc),
		sem:             make(chan struct{}, cfg.MaxConcurrent),
		storageFailedAt: make(map[string]time.Time),
		pausedTasks:     make(map[string]struct{}),
	}
}

// markPaused / clearPaused / isPaused guard the deliberate-pause set. See the
// pausedTasks field comment for why a pause must not drop the resume entry.
func (m *Manager) markPaused(taskID string) {
	m.pausedMu.Lock()
	m.pausedTasks[taskID] = struct{}{}
	m.pausedMu.Unlock()
}

func (m *Manager) clearPaused(taskID string) {
	m.pausedMu.Lock()
	delete(m.pausedTasks, taskID)
	m.pausedMu.Unlock()
}

func (m *Manager) isPaused(taskID string) bool {
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	_, ok := m.pausedTasks[taskID]
	return ok
}

// PausedTaskIDs returns the IDs currently held in the deliberate-pause set.
// Read by the local control server so `unarr downloads` can tell a paused task
// apart from one that simply is not running.
func (m *Manager) PausedTaskIDs() []string {
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	ids := make([]string, 0, len(m.pausedTasks))
	for id := range m.pausedTasks {
		ids = append(ids, id)
	}
	return ids
}

// DropResume removes a task from the resume store without touching a running
// download. It is how a zombie is put down: a task the server no longer knows
// about (deleted row) is not in m.active after a restart is re-submitted from
// the store forever, so the entry itself has to go. Safe when no store is wired.
func (m *Manager) DropResume(taskID string) {
	if m.taskStore != nil {
		m.taskStore.Remove(taskID)
	}
	m.clearPaused(taskID)
}

// Submit queues a task for download. Non-blocking if capacity available.
func (m *Manager) Submit(ctx context.Context, at agent.Task) {
	// Storage-failure cooldown: refuse to re-run a task that just failed writing
	// to its destination. Between the agent reporting `failed` and the server
	// persisting it, an in-flight sync can re-claim the still-"pending" row and
	// re-dispatch it — a burst of full re-downloads to the same broken mount. A
	// force-started task bypasses this (an explicit user "force start" is fresh
	// intent). A normal manual Retry from the web clears the error and re-pends
	// minutes later (after the user fixes storage), well past the cooldown.
	if !at.ForceStart && m.inStorageCooldown(at.ID) {
		log.Printf("[%s] skipping re-dispatch - failed writing to storage <%s ago (fix your download folder, then retry)",
			agent.ShortID(at.ID), storageFailCooldown)
		return
	}

	task := NewTaskFromAgent(at)
	// Event-driven uplink: push every status transition to the server immediately.
	task.SetOnChange(m.OnStateChange)

	// Per-task cancellable context so CancelTask can unblock the goroutine
	taskCtx, taskCancel := context.WithCancel(ctx)

	m.activeMu.Lock()
	// Dedup: a task can arrive twice — once when the daemon re-submits it from
	// the resume store on startup, and again when the web re-dispatches it. The
	// second arrival must NOT launch a parallel goroutine for the same files.
	if _, exists := m.active[task.ID]; exists {
		m.activeMu.Unlock()
		taskCancel()
		log.Printf("[%s] already active - ignoring duplicate submit", agent.ShortID(task.ID))
		return
	}
	m.active[task.ID] = task
	m.cancels[task.ID] = taskCancel
	m.activeMu.Unlock()

	// A submit is a fresh run: whatever pause held this ID is over. Leaving it in
	// the set would make the NEXT genuine terminal keep a stale resume entry.
	m.clearPaused(task.ID)

	// Persist real downloads so a daemon restart can resume them (torrent via
	// the piece-completion DB, debrid via Range, usenet via its tracker). Stream
	// and seed-file tasks are transient — not resumed. Upgrade downloads
	// (ReplacePath set) are excluded too: re-running one after an interrupted
	// organize could double-download or replace the wrong target.
	if m.taskStore != nil && (at.Mode == "" || at.Mode == "download") && at.ReplacePath == "" {
		m.taskStore.Add(at)
	}

	m.reporter.Track(task)

	// Force start: bypass semaphore (like Transmission's "Force Start")
	if at.ForceStart {
		log.Printf("[%s] force start: bypassing queue", agent.ShortID(task.ID))
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer taskCancel()
			m.processTask(taskCtx, task)
		}()
		return
	}

	// Acquire semaphore slot
	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		taskCancel()
		return
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			<-m.sem
			if m.OnTaskDone != nil {
				m.OnTaskDone()
			}
		}()
		defer taskCancel()
		m.processTask(taskCtx, task)
	}()
}

// HasCapacity returns true if there's room for more downloads.
func (m *Manager) HasCapacity() bool {
	return len(m.sem) < cap(m.sem)
}

// FreeSlots returns the number of available download slots.
func (m *Manager) FreeSlots() int {
	return cap(m.sem) - len(m.sem)
}

// ActiveCount returns the number of in-progress downloads.
func (m *Manager) ActiveCount() int {
	m.activeMu.RLock()
	defer m.activeMu.RUnlock()
	return len(m.active)
}

// GetTask returns a single active task by ID, or nil.
func (m *Manager) GetTask(taskID string) *Task {
	m.activeMu.RLock()
	defer m.activeMu.RUnlock()
	return m.active[taskID]
}

// ActiveTaskIDs returns the IDs of all in-progress tasks.
func (m *Manager) ActiveTaskIDs() []string {
	m.activeMu.RLock()
	defer m.activeMu.RUnlock()
	ids := make([]string, 0, len(m.active))
	for id := range m.active {
		ids = append(ids, id)
	}
	return ids
}

// ActiveTasks returns a snapshot of all active tasks.
func (m *Manager) ActiveTasks() []*Task {
	m.activeMu.RLock()
	defer m.activeMu.RUnlock()
	tasks := make([]*Task, 0, len(m.active))
	for _, t := range m.active {
		tasks = append(tasks, t)
	}
	return tasks
}

// TaskStates returns the current state of all active tasks plus any recently
// finished tasks that haven't been synced yet. Called by the sync goroutine.
func (m *Manager) TaskStates() []agent.TaskState {
	// Collect active tasks
	m.activeMu.RLock()
	states := make([]agent.TaskState, 0, len(m.active))
	for _, t := range m.active {
		states = append(states, agent.TaskStateFromUpdate(t.ToStatusUpdate()))
	}
	m.activeMu.RUnlock()

	// Drain recently finished tasks (consumed once per sync)
	m.recentMu.Lock()
	states = append(states, m.recentFinished...)
	m.recentFinished = nil
	m.recentMu.Unlock()

	return states
}

// recordFinished stores a completed/failed task for the next sync cycle and drops
// it from the resume store (a genuine terminal). See recordFinishedKeep for the
// resume-preserving variant.
func (m *Manager) recordFinished(update agent.StatusUpdate) {
	m.recordFinishedKeep(update, false)
}

// recordFinishedKeep reports a task's final state to the next sync cycle. When
// keepResume is false it also drops the resume-store entry on a genuine terminal
// state (completed / failed / user-cancelled). A shutdown-interrupted task and a
// VPN-paused task (keepResume=true) KEEP the entry so the daemon re-submits and
// resumes them on the next start.
func (m *Manager) recordFinishedKeep(update agent.StatusUpdate, keepResume bool) {
	// A deliberately paused task unwinds through fail() (its context was
	// cancelled), which would otherwise drop the resume entry and make the
	// pause unresumable. See pausedTasks.
	keep := keepResume || m.isPaused(update.TaskID)
	if m.taskStore != nil && !m.shuttingDown.Load() && !keep {
		m.taskStore.Remove(update.TaskID)
	}

	m.recentMu.Lock()
	defer m.recentMu.Unlock()
	m.recentFinished = append(m.recentFinished, agent.TaskStateFromUpdate(update))
	// Keep bounded
	if len(m.recentFinished) > 20 {
		m.recentFinished = m.recentFinished[len(m.recentFinished)-20:]
	}
}

// CancelTask cancels an active download by task ID (keeps partial files).
// Reports whether a running task was actually stopped — false means the ID is
// not active here, which the caller turns into "not found" (or, for a zombie,
// into a resume-store drop via DropResume).
func (m *Manager) CancelTask(taskID string) bool {
	m.activeMu.RLock()
	task, ok := m.active[taskID]
	cancel := m.cancels[taskID]
	m.activeMu.RUnlock()

	if !ok {
		return false
	}

	// A cancel is terminal: the resume entry must go BEFORE the goroutine
	// unwinds, or a shutdown racing the cancel (shuttingDown gates the removal
	// in recordFinished) would leave the entry behind and the daemon would
	// resurrect the download on its next start.
	m.clearPaused(taskID)
	if m.taskStore != nil {
		m.taskStore.Remove(taskID)
	}

	// Cancel the task's context first — this unblocks the goroutine
	// (e.g. stuck waiting for metadata) so it exits and releases the semaphore slot.
	if cancel != nil {
		cancel()
	}

	if dl, exists := m.downloaders[task.GetResolvedMethod()]; exists {
		dl.Pause(taskID) // stop download, keep files
	}

	task.SetError("cancelled by user")
	task.Transition(StatusCancelled)

	log.Printf("[%s] cancelled: %s", agent.ShortID(taskID), task.Title)
	return true
}

// PauseTask pauses an active download (keeps partial files for resume).
// Reports whether a running task was actually paused.
func (m *Manager) PauseTask(taskID string) bool {
	m.activeMu.RLock()
	task, ok := m.active[taskID]
	cancel := m.cancels[taskID]
	m.activeMu.RUnlock()

	if !ok {
		return false
	}

	// Mark BEFORE cancelling the context: the goroutine unwinds through fail(),
	// and recordFinished consults this set to keep the resume entry.
	m.markPaused(taskID)

	if cancel != nil {
		cancel()
	}

	if dl, exists := m.downloaders[task.GetResolvedMethod()]; exists {
		dl.Pause(taskID) // stop download, keep files for resume
	}

	task.Transition(StatusCancelled) // will be re-created as pending by server
	log.Printf("[%s] paused: %s", agent.ShortID(taskID), task.Title)
	return true
}

// CancelAndDeleteFiles cancels a download and removes its files from disk.
// Reports whether a running task was actually stopped.
func (m *Manager) CancelAndDeleteFiles(taskID string) bool {
	m.activeMu.RLock()
	task, ok := m.active[taskID]
	cancel := m.cancels[taskID]
	m.activeMu.RUnlock()

	if !ok {
		return false
	}

	// Terminal — drop the resume entry up front, same reasoning as CancelTask.
	m.clearPaused(taskID)
	if m.taskStore != nil {
		m.taskStore.Remove(taskID)
	}

	if cancel != nil {
		cancel()
	}

	if dl, exists := m.downloaders[task.GetResolvedMethod()]; exists {
		dl.Cancel(taskID) // stop download + delete files
	}

	task.SetError("cancelled by user")
	task.Transition(StatusCancelled)

	log.Printf("[%s] cancelled + files deleted: %s", agent.ShortID(taskID), task.Title)
	return true
}

// Wait blocks until all active downloads finish.
func (m *Manager) Wait() {
	m.wg.Wait()
}

// Shutdown stops accepting tasks and waits for active downloads to finish.
func (m *Manager) Shutdown(ctx context.Context) {
	// Flag shutdown BEFORE cancelling task contexts: tasks interrupted by the
	// shutdown then keep their resume-store entry (recordFinished skips the
	// removal) so the daemon re-submits and resumes them on the next start.
	m.shuttingDown.Store(true)

	// Cancel every task context NOW (before waiting). Downloads block on their
	// context, so this is what actually unblocks them — and because shuttingDown
	// is already set, their recordFinished keeps the resume entry. (Waiting first
	// would just stall until the timeout, and relying on the daemon's outer ctx
	// cancel would race ahead of shuttingDown and wipe the entries.)
	m.activeMu.Lock()
	for id, cancel := range m.cancels {
		cancel()
		delete(m.cancels, id)
	}
	m.activeMu.Unlock()

	// Wait for goroutines with timeout
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		log.Println("shutdown timeout, abandoning active downloads")
	}

	// Shutdown all downloaders
	for _, d := range m.downloaders {
		if err := d.Shutdown(ctx); err != nil {
			log.Printf("downloader shutdown: %v", err)
		}
	}

	m.activeMu.Lock()
	m.active = make(map[string]*Task)
	m.activeMu.Unlock()
}

func (m *Manager) processTask(ctx context.Context, task *Task) {
	defer func() {
		m.activeMu.Lock()
		delete(m.active, task.ID)
		delete(m.cancels, task.ID)
		m.activeMu.Unlock()
	}()

	// On a corrupt/truncated result (a downloader's own integrity guard, or the
	// shared on-disk verify below), re-download the SAME source a bounded number
	// of times — a fresh clean-start attempt usually lands intact (the 2026-06-15
	// debrid NFS write-back truncation was transient). Only after exhausting the
	// retries is the task surfaced as damaged, so "completed" NEVER means a corrupt
	// file. (User-chosen "both" policy: auto-retry, then visible-damaged.)
	const maxIntegrityAttempts = 3

	// A StorageError (fsync/close I/O error, stalled NFS/SMB mount) means the
	// DESTINATION failed, not the bytes — re-downloading writes to the same broken
	// mount and burns bandwidth. Retry just ONCE (a mount can briefly stall and
	// recover), then PAUSE as resumable with a storage-specific message. Counted
	// SEPARATELY from integrity attempts so a storage stall never eats the
	// corruption-retry budget (and vice-versa). Incident 2026-07-24: a debrid
	// download of a healthy file looped because the NFS server timed out on the
	// final fsync — the old code mislabeled it flush_failed integrity corruption.
	const maxStorageAttempts = 2
	storageAttempt := 0

	for attempt := 1; ; attempt++ {
		result, err := m.attemptDownload(ctx, task)
		if err != nil {
			if IsInsufficientDisk(err) {
				// Terminal — another source would fill the same disk.
				m.fail(ctx, task, err.Error())
				return
			}
			if IsStorage(err) {
				storageAttempt++
				if storageAttempt < maxStorageAttempts {
					log.Printf("[%s] storage write failed (attempt %d/%d), retrying once in case the mount recovered: %v",
						agent.ShortID(task.ID), storageAttempt, maxStorageAttempts, err)
					continue
				}
				m.failStorage(ctx, task, err)
				return
			}
			if IsIntegrity(err) {
				if attempt < maxIntegrityAttempts {
					log.Printf("[%s] integrity check failed (attempt %d/%d), re-downloading clean: %v",
						agent.ShortID(task.ID), attempt, maxIntegrityAttempts, err)
					continue
				}
				m.failDamaged(ctx, task, err)
				return
			}
			// VPN kill-switch tripped mid-download and no safe fallback completed it:
			// PAUSE the task (keep the partial + resume-store entry) instead of
			// hard-failing, so it resumes once the tunnel heals. See pauseForVPN.
			if errors.Is(err, ErrVPNTunnelDown) {
				m.pauseForVPN(ctx, task)
				return
			}
			m.fail(ctx, task, err.Error())
			return
		}

		// Shared on-disk safety net across every backend — the last line of defense
		// against a truncated/short file slipping past a downloader's own checks.
		if err := task.Transition(StatusVerifying); err != nil {
			m.fail(ctx, task, "transition error: "+err.Error())
			return
		}
		if verr := verify(result); verr != nil {
			// A storage failure during verify (stat over a stalled mount) is the
			// destination faulting, not corrupt bytes — retry once, then pause.
			if IsStorage(verr) {
				storageAttempt++
				if storageAttempt < maxStorageAttempts {
					log.Printf("[%s] storage read-back failed on verify (attempt %d/%d), retrying once: %v",
						agent.ShortID(task.ID), storageAttempt, maxStorageAttempts, verr)
					continue
				}
				m.failStorage(ctx, task, verr)
				return
			}
			if IsIntegrity(verr) {
				removeBrokenResult(task.ID, result) // clean start so a resume doesn't append to a short file
				if attempt < maxIntegrityAttempts {
					log.Printf("[%s] verify failed (attempt %d/%d), re-downloading clean: %v",
						agent.ShortID(task.ID), attempt, maxIntegrityAttempts, verr)
					continue
				}
				m.failDamaged(ctx, task, verr)
				return
			}
			m.fail(ctx, task, "verification failed: "+verr.Error())
			return
		}

		m.finalizeVerified(ctx, task, result)
		return
	}
}

// attemptDownload resolves a method and downloads once, falling back to the next
// configured method on a plain transport failure (NOT on disk-full or integrity
// failures — those are the caller's to handle). Returns the download Result.
func (m *Manager) attemptDownload(ctx context.Context, task *Task) (*Result, error) {
	if err := task.Transition(StatusResolving); err != nil {
		return nil, fmt.Errorf("transition error: %w", err)
	}
	method, err := resolveMethod(ctx, task, m.downloaders, m.cfg.PreferredMethods)
	if err != nil {
		return nil, fmt.Errorf("no method available: %w", err)
	}
	task.SetResolvedMethod(method)
	log.Printf("[%s] resolved method: %s", agent.ShortID(task.ID), method)

	if err := task.Transition(StatusDownloading); err != nil {
		return nil, fmt.Errorf("transition error: %w", err)
	}
	result, err := m.runDownload(ctx, task, method)
	if err != nil {
		// Disk-full is terminal; an integrity failure is retried in-place by the
		// caller (same source, clean start); a storage failure means the target
		// mount faulted (another method writes to the same dir) — don't burn the
		// method fallback on any of them. Only a plain transport failure tries next.
		if IsInsufficientDisk(err) || IsIntegrity(err) || IsStorage(err) {
			return nil, err
		}
		// ErrVPNTunnelDown: the tunnel died mid-download (torrent dropped, partial
		// kept). Prefer a genuinely-available SAFE fallback (debrid/usenet, HTTPS/NNTP
		// — no swarm) if the agent has one; if none can complete it, re-surface the
		// VPN cause so processTask PAUSES the task as resumable instead of failing it.
		vpnDown := errors.Is(err, ErrVPNTunnelDown)
		if tryFallback(task, m.downloaders, m.cfg.PreferredMethods) {
			log.Printf("[%s] %s failed, trying fallback: %v", agent.ShortID(task.ID), method, err)
			if terr := task.Transition(StatusResolving); terr != nil {
				return nil, err
			}
			res, ferr := m.attemptFallback(ctx, task)
			if ferr != nil && vpnDown {
				return nil, ErrVPNTunnelDown // no safe method available — keep the torrent resumable
			}
			return res, ferr
		}
		return nil, err
	}
	return result, nil
}

// attemptFallback runs the next available method after a transport failure.
func (m *Manager) attemptFallback(ctx context.Context, task *Task) (*Result, error) {
	method, err := resolveMethod(ctx, task, m.downloaders, m.cfg.PreferredMethods)
	if err != nil {
		return nil, fmt.Errorf("fallback failed: %w", err)
	}
	task.SetResolvedMethod(method)
	log.Printf("[%s] fallback to: %s", agent.ShortID(task.ID), method)
	if err := task.Transition(StatusDownloading); err != nil {
		return nil, fmt.Errorf("transition error: %w", err)
	}
	return m.runDownload(ctx, task, method)
}

// runDownload invokes a single downloader, draining its progress channel.
func (m *Manager) runDownload(ctx context.Context, task *Task, method DownloadMethod) (*Result, error) {
	progressCh := make(chan Progress, 16)
	// Drain progress channel (reporter reads progress directly from the task).
	go func() {
		for range progressCh {
		}
	}()
	dl := m.downloaders[method]
	result, err := dl.Download(ctx, task, m.cfg.OutputDir, progressCh)
	close(progressCh)
	return result, err
}

// removeBrokenResult deletes a single-file result that failed the on-disk verify
// so the retry's downloader starts clean (debrid resumes from a partial via HTTP
// Range — appending to a truncated stub would compound the corruption). Multi-file
// (directory) results are left for the downloader/anacrolix to re-verify in place.
func removeBrokenResult(taskID string, result *Result) {
	if result == nil || result.FilePath == "" {
		return
	}
	if fi, err := os.Stat(result.FilePath); err == nil && !fi.IsDir() {
		if rmErr := os.Remove(result.FilePath); rmErr != nil {
			log.Printf("[%s] failed to remove broken file %s: %v", agent.ShortID(taskID), result.FilePath, rmErr)
		}
	}
}

// extractPackedRelease unpacks a scene-style archived release in place so the
// subsequent organize() finds a real video instead of a folder of .rNN parts.
//
// Deliberately non-fatal in every failure mode. Unlike usenet — where the
// archive IS the delivery format and an unextracted payload is useless — a
// torrent's raw parts are exactly what the swarm served. Failing the task here
// would downgrade "we could not improve this" into "you get nothing", losing a
// download that completed and verified fine. Every problem is logged and the
// raw release is left for organize()'s existing move-the-folder fallback.
func (m *Manager) extractPackedRelease(task *Task, result *Result) {
	if result == nil || result.FilePath == "" {
		return
	}
	// Only a multi-file (directory) result can be a packed release; a
	// single-file result is already the payload.
	fi, err := os.Stat(result.FilePath)
	if err != nil || !fi.IsDir() {
		return
	}

	shortID := agent.ShortID(task.ID)

	// NzbPassword is the only password the task carries. It is usenet-sourced,
	// but a user who typed one for a release is describing THAT release, so
	// honour it here too rather than adding a second field that means the same
	// thing. Empty for the overwhelming majority of torrents.
	res, err := postprocess.ExtractInDir(result.FilePath, task.NzbPassword)
	if err != nil {
		if _, ok := err.(*postprocess.PasswordError); ok {
			log.Printf("[%s] release is password protected - leaving it packed (set a password in download options to unpack)", shortID)
			return
		}
		log.Printf("[%s] extract failed, leaving release packed: %v", shortID, err)
		return
	}
	if res.Note != "" {
		log.Printf("[%s] %s", shortID, res.Note)
		return
	}
	if !res.Extracted {
		return // no archive in there: the common case
	}

	log.Printf("[%s] extracted packed release (%d file(s))", shortID, len(res.Files))

	// Keep the archive parts while seeding: a seeding torrent serves the EXACT
	// files it downloaded, so deleting the .rNN volumes would break the swarm's
	// requests and, on a private tracker, the user's ratio obligation. The unusable
	// pile of parts was the whole complaint here, but "unusable" beats "un-seedable"
	// — the extracted video sits beside them and organize() moves that out, so the
	// user gets a playable file either way. Reclaiming the space is seedAndDrop's
	// business once it stops seeding, not ours mid-flight.
	if m.cfg.SeedEnabled {
		log.Printf("[%s] keeping archive parts: torrent is seeding", shortID)
		return
	}

	// Not seeding: drop the parts, but only after a SUCCESSFUL extraction, so a
	// release that failed to unpack keeps everything the user was served.
	if err := postprocess.CleanupArchives(result.FilePath); err != nil {
		log.Printf("[%s] archive cleanup warning: %v", shortID, err)
	}
}

// finalizeVerified runs organize → upgrade replacement → complete for a download
// that already passed verify.
func (m *Manager) finalizeVerified(ctx context.Context, task *Task, result *Result) {
	// Organize
	if err := task.Transition(StatusOrganizing); err != nil {
		m.fail(ctx, task, "transition error: "+err.Error())
		return
	}

	// Unpack a packed release BEFORE organizing. organize() picks the principal
	// video out of a release dir; with the payload still inside a .rar set there
	// is no video to pick, so it fell back to moving the raw folder and the user
	// got a pile of .r00/.r01 files instead of something playable.
	//
	// Usenet is excluded: its own pipeline already extracted (and par2-verified)
	// before it ever reaches here — running it twice would find no archive and
	// merely waste a directory scan, but the exclusion keeps the ownership of
	// each method's post-processing unambiguous.
	if result.Method != MethodUsenet {
		m.extractPackedRelease(task, result)
	}

	finalPath, err := organize(result, task, m.cfg.Organize)
	if err != nil {
		// A failed organize is NOT a completed download: the file is stranded in the
		// download dir under a raw release name (or half-moved), yet the old code
		// logged a warning and reported "completed" — so the library showed a green
		// item pointing at a file that was never filed into Movies/TV (a lie to the
		// user + a source of the junk-in-download-dir the reconcile sweep now cleans).
		// Surface it as FAILED with the organize error so it's visibly retryable.
		// (organize returns (path, nil) — same path, no error — when disabled or a
		// no-op; that path is NOT an error and still completes below.)
		m.fail(ctx, task, "organize failed: "+err.Error())
		return
	}
	if finalPath == "" {
		finalPath = result.FilePath
	}
	task.SetFilePath(finalPath)

	// Handle upgrade replacement (mode = "upgrade")
	if task.ReplacePath != "" {
		backupDir := "" // uses default ~/.local/share/unarr/replaced/
		if err := replaceFile(task.ReplacePath, finalPath, backupDir); err != nil {
			log.Printf("[%s] replace warning: %v (keeping new file at %s)", agent.ShortID(task.ID), err, finalPath)
		} else {
			task.SetFilePath(task.ReplacePath)
			log.Printf("[%s] upgraded: replaced %s", agent.ShortID(task.ID), task.ReplacePath)
		}
	}

	// Complete
	if err := task.Transition(StatusCompleted); err != nil {
		m.fail(ctx, task, "transition error: "+err.Error())
		return
	}
	log.Printf("[%s] completed: %s -> %s", agent.ShortID(task.ID), task.Title, finalPath)
	if m.cfg.Notifications {
		notify.Send("Download complete", task.Title)
	}
	m.recordFinished(task.ToStatusUpdate())
	m.reporter.ReportFinal(ctx, task)
}

func (m *Manager) fail(ctx context.Context, task *Task, msg string) {
	task.SetError(msg)
	task.Transition(StatusFailed)
	log.Printf("[%s] FAILED: %s - %s", agent.ShortID(task.ID), task.Title, msg)
	if m.cfg.Notifications {
		notify.Send("Download failed", task.Title+": "+msg)
	}
	m.recordFinished(task.ToStatusUpdate())
	m.reporter.ReportFinal(ctx, task)
}

// pauseForVPN handles a mid-download tunnel loss on a task with no safe fallback
// (torrent-only, or an auto agent whose debrid/usenet couldn't complete it). The
// kill-switch already dropped the torrent KEEPING the partial files on disk; we
// surface it as PAUSED (StatusCancelled), not FAILED, so it stays resumable:
//   - the resume-store entry is KEPT, so a daemon restart re-submits and resumes it
//     from the kept pieces (anacrolix piece-completion DB);
//   - the web sees StatusCancelled and re-creates it as pending, so a re-dispatch
//     once the tunnel heals resumes it too (the Available() gate holds torrent off
//     until the VPN supervisor brings the tunnel back).
//
// Deliberate limit (avoids coupling the manager to the VPN supervisor, which the
// objective flagged as over-engineering): the manager does NOT itself re-queue on
// the reconnect signal — in-session torrent-only resume relies on the web
// re-dispatch; a daemon restart resumes unconditionally. Safety is intact: no
// clear-net P2P and no data loss (partial kept).
func (m *Manager) pauseForVPN(ctx context.Context, task *Task) {
	task.SetError("VPN tunnel down — torrent paused (partial kept); resumes when the tunnel recovers")
	if err := task.Transition(StatusCancelled); err != nil {
		// The state machine rejected the pause (unexpected from downloading/resolving)
		// — fall back to a plain fail so the task never sticks in a non-terminal state.
		m.fail(ctx, task, "VPN tunnel down: "+err.Error())
		return
	}
	log.Printf("[%s] VPN tunnel down - torrent PAUSED (partial kept, resumes when the tunnel heals): %s",
		agent.ShortID(task.ID), task.Title)
	m.recordFinishedKeep(task.ToStatusUpdate(), true) // report paused state; KEEP the resume-store entry
	m.reporter.ReportFinal(ctx, task)
}

// damagedErrorPrefix is a STABLE marker the web matches on (download_task.error_message)
// to render a "corrupt — re-download" affordance instead of a generic failure. Keep
// in sync with the web's detection (src/lib/services/agent.ts / downloads UI).
const damagedErrorPrefix = "corrupt download: "

// failDamaged marks a task failed after its bytes repeatedly failed the integrity
// check (truncated/short file, checksum/par2 failure). Same terminal path as fail,
// but with the damagedErrorPrefix so the web can surface a re-download CTA — the
// download_task table has no integrity column, so the message IS the signal.
func (m *Manager) failDamaged(ctx context.Context, task *Task, err error) {
	m.fail(ctx, task, damagedErrorPrefix+err.Error())
}

// storageErrorPrefix is a STABLE marker the web matches on (download_task.error_message)
// to render a "check your download folder / storage" affordance instead of the
// "corrupt — re-download" one: the bytes were fine, the DESTINATION failed, so a
// re-download is the wrong CTA. Keep in sync with the web's detection
// (src/lib/services/agent.ts / downloads UI).
const storageErrorPrefix = "storage unavailable: "

// failStorage handles a persistent write failure to the target directory (a
// stalled/dropped NFS/SMB mount, a read-only or disconnected volume) after the
// single storage retry didn't recover.
//
// It reports a TERMINAL failed state carrying storageErrorPrefix — deliberately
// NOT a StatusCancelled "pause". A cancelled state is never reported to the
// server (ToStatusUpdate maps it to an empty apiStatus), so a "storage pause"
// would leave the task claimable and the server's claimPendingTasks would re-hand
// it to the agent every sync cycle — a re-download loop with no gate to stop it
// (unlike pauseForVPN, which the torrent Available() gate holds off). A terminal
// failed IS reported and persisted, so the loop can't form: the web shows the
// storage message (amber, "check your drive/NAS") with a Retry button, and the
// user re-runs it after fixing their storage. Distinct from failDamaged on
// purpose — the bytes were fine, so the wording must not say "corrupt".
// (Incident 2026-07-24: NFS soft-mount fsync timeout looping a debrid download.)
func (m *Manager) failStorage(ctx context.Context, task *Task, cause error) {
	log.Printf("[%s] storage unavailable - FAILED (retry after fixing your download folder): %s - %s",
		agent.ShortID(task.ID), task.Title, cause.Error())
	if m.cfg.Notifications {
		notify.Send("Download failed — storage unavailable",
			task.Title+": could not write to your download folder. Check the drive/NAS, then retry.")
	}
	// Arm the Submit cooldown BEFORE reporting failed: the server may re-hand this
	// task on the very next sync (racing the failed it hasn't persisted yet), and
	// the cooldown must already be in place to refuse it.
	m.markStorageFailed(task.ID)
	m.fail(ctx, task, storageErrorPrefix+cause.Error())
}

// markStorageFailed stamps a task as just-failed-on-storage and opportunistically
// evicts stamps older than the cooldown, so the map can't grow without bound.
func (m *Manager) markStorageFailed(taskID string) {
	now := time.Now()
	m.storageFailMu.Lock()
	defer m.storageFailMu.Unlock()
	for id, t := range m.storageFailedAt {
		if now.Sub(t) > storageFailCooldown {
			delete(m.storageFailedAt, id)
		}
	}
	m.storageFailedAt[taskID] = now
}

// inStorageCooldown reports whether taskID failed on storage within the cooldown
// window — i.e. Submit should refuse to re-run it right now. An expired stamp is
// evicted here too (not only in markStorageFailed), so a one-off storage failure
// with no later failures to trigger the sweep can't linger in the map forever.
func (m *Manager) inStorageCooldown(taskID string) bool {
	m.storageFailMu.Lock()
	defer m.storageFailMu.Unlock()
	t, ok := m.storageFailedAt[taskID]
	if !ok {
		return false
	}
	if time.Since(t) >= storageFailCooldown {
		delete(m.storageFailedAt, taskID)
		return false
	}
	return true
}
