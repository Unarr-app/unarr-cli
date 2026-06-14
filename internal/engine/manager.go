package engine

import (
	"context"
	"log"
	"sync"
	"sync/atomic"

	"github.com/torrentclaw/unarr/internal/agent"
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
}

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
		cfg:         cfg,
		reporter:    reporter,
		downloaders: dlMap,
		active:      make(map[string]*Task),
		cancels:     make(map[string]context.CancelFunc),
		sem:         make(chan struct{}, cfg.MaxConcurrent),
	}
}

// Submit queues a task for download. Non-blocking if capacity available.
func (m *Manager) Submit(ctx context.Context, at agent.Task) {
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
		log.Printf("[%s] already active — ignoring duplicate submit", agent.ShortID(task.ID))
		return
	}
	m.active[task.ID] = task
	m.cancels[task.ID] = taskCancel
	m.activeMu.Unlock()

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

// recordFinished stores a completed/failed task for the next sync cycle.
func (m *Manager) recordFinished(update agent.StatusUpdate) {
	// Drop from the resume store on a genuine terminal state (completed / failed
	// / user-cancelled). A shutdown-interrupted task is NOT removed — it stays so
	// the daemon re-submits and resumes it on the next start.
	if m.taskStore != nil && !m.shuttingDown.Load() {
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
func (m *Manager) CancelTask(taskID string) {
	m.activeMu.RLock()
	task, ok := m.active[taskID]
	cancel := m.cancels[taskID]
	m.activeMu.RUnlock()

	if !ok {
		return
	}

	// Cancel the task's context first — this unblocks the goroutine
	// (e.g. stuck waiting for metadata) so it exits and releases the semaphore slot.
	if cancel != nil {
		cancel()
	}

	if dl, exists := m.downloaders[task.ResolvedMethod]; exists {
		dl.Pause(taskID) // stop download, keep files
	}

	task.mu.Lock()
	task.ErrorMessage = "cancelled by user"
	task.mu.Unlock()
	task.Transition(StatusCancelled)

	log.Printf("[%s] cancelled: %s", agent.ShortID(taskID), task.Title)
}

// PauseTask pauses an active download (keeps partial files for resume).
func (m *Manager) PauseTask(taskID string) {
	m.activeMu.RLock()
	task, ok := m.active[taskID]
	cancel := m.cancels[taskID]
	m.activeMu.RUnlock()

	if !ok {
		return
	}

	if cancel != nil {
		cancel()
	}

	if dl, exists := m.downloaders[task.ResolvedMethod]; exists {
		dl.Pause(taskID) // stop download, keep files for resume
	}

	task.Transition(StatusCancelled) // will be re-created as pending by server
	log.Printf("[%s] paused: %s", agent.ShortID(taskID), task.Title)
}

// CancelAndDeleteFiles cancels a download and removes its files from disk.
func (m *Manager) CancelAndDeleteFiles(taskID string) {
	m.activeMu.RLock()
	task, ok := m.active[taskID]
	cancel := m.cancels[taskID]
	m.activeMu.RUnlock()

	if !ok {
		return
	}

	if cancel != nil {
		cancel()
	}

	if dl, exists := m.downloaders[task.ResolvedMethod]; exists {
		dl.Cancel(taskID) // stop download + delete files
	}

	task.mu.Lock()
	task.ErrorMessage = "cancelled by user"
	task.mu.Unlock()
	task.Transition(StatusCancelled)

	log.Printf("[%s] cancelled + files deleted: %s", agent.ShortID(taskID), task.Title)
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

	// 1. Resolve method
	if err := task.Transition(StatusResolving); err != nil {
		m.fail(ctx, task, "transition error: "+err.Error())
		return
	}

	method, err := resolveMethod(ctx, task, m.downloaders, m.cfg.PreferredMethods)
	if err != nil {
		m.fail(ctx, task, "no method available: "+err.Error())
		return
	}

	task.ResolvedMethod = method
	log.Printf("[%s] resolved method: %s", agent.ShortID(task.ID), method)

	// 2. Download
	if err := task.Transition(StatusDownloading); err != nil {
		m.fail(ctx, task, "transition error: "+err.Error())
		return
	}

	progressCh := make(chan Progress, 16)

	// Drain progress channel (just for logging; reporter reads directly from task)
	go func() {
		for range progressCh {
			// Progress already applied via task.UpdateProgress in the downloader
		}
	}()

	dl := m.downloaders[method]
	result, err := dl.Download(ctx, task, m.cfg.OutputDir, progressCh)
	close(progressCh)

	if err != nil {
		// A full disk is terminal — another source would fill the same disk, so
		// skip the fallback and surface the clear message immediately.
		if IsInsufficientDisk(err) {
			m.fail(ctx, task, err.Error())
			return
		}
		// Try fallback
		if tryFallback(task, m.downloaders, m.cfg.PreferredMethods) {
			log.Printf("[%s] %s failed, trying fallback: %v", agent.ShortID(task.ID), method, err)
			if err := task.Transition(StatusResolving); err == nil {
				m.processTaskRetry(ctx, task)
				return
			}
		}
		m.fail(ctx, task, err.Error())
		return
	}

	m.finalize(ctx, task, result)
}

// processTaskRetry handles fallback after a method failure.
func (m *Manager) processTaskRetry(ctx context.Context, task *Task) {
	method, err := resolveMethod(ctx, task, m.downloaders, m.cfg.PreferredMethods)
	if err != nil {
		m.fail(ctx, task, "fallback failed: "+err.Error())
		return
	}

	task.ResolvedMethod = method
	log.Printf("[%s] fallback to: %s", agent.ShortID(task.ID), method)

	if err := task.Transition(StatusDownloading); err != nil {
		m.fail(ctx, task, "transition error: "+err.Error())
		return
	}

	progressCh := make(chan Progress, 16)
	go func() {
		for range progressCh {
		}
	}()

	dl := m.downloaders[method]
	result, err := dl.Download(ctx, task, m.cfg.OutputDir, progressCh)
	close(progressCh)

	if err != nil {
		// No further fallback here — same disk, same outcome — so an
		// InsufficientDiskError on the fallback surfaces its message directly.
		m.fail(ctx, task, err.Error())
		return
	}

	m.finalize(ctx, task, result)
}

// finalize runs verify → organize → upgrade replacement → complete for a downloaded task.
func (m *Manager) finalize(ctx context.Context, task *Task, result *Result) {
	// Verify
	if err := task.Transition(StatusVerifying); err != nil {
		m.fail(ctx, task, "transition error: "+err.Error())
		return
	}
	if err := verify(result); err != nil {
		m.fail(ctx, task, "verification failed: "+err.Error())
		return
	}

	// Organize
	if err := task.Transition(StatusOrganizing); err != nil {
		m.fail(ctx, task, "transition error: "+err.Error())
		return
	}
	finalPath, err := organize(result, task, m.cfg.Organize)
	if err != nil {
		log.Printf("[%s] organize warning: %v (keeping in download dir)", agent.ShortID(task.ID), err)
		finalPath = result.FilePath
	}
	if finalPath == "" {
		finalPath = result.FilePath
	}
	task.mu.Lock()
	task.FilePath = finalPath
	task.mu.Unlock()

	// Handle upgrade replacement (mode = "upgrade")
	if task.ReplacePath != "" {
		backupDir := "" // uses default ~/.local/share/unarr/replaced/
		if err := replaceFile(task.ReplacePath, finalPath, backupDir); err != nil {
			log.Printf("[%s] replace warning: %v (keeping new file at %s)", agent.ShortID(task.ID), err, finalPath)
		} else {
			task.mu.Lock()
			task.FilePath = task.ReplacePath
			task.mu.Unlock()
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
		desktopNotify("Download complete", task.Title)
	}
	m.recordFinished(task.ToStatusUpdate())
	m.reporter.ReportFinal(ctx, task)
}

func (m *Manager) fail(ctx context.Context, task *Task, msg string) {
	task.mu.Lock()
	task.ErrorMessage = msg
	task.mu.Unlock()
	task.Transition(StatusFailed)
	log.Printf("[%s] FAILED: %s — %s", agent.ShortID(task.ID), task.Title, msg)
	if m.cfg.Notifications {
		desktopNotify("Download failed", task.Title+": "+msg)
	}
	m.recordFinished(task.ToStatusUpdate())
	m.reporter.ReportFinal(ctx, task)
}
