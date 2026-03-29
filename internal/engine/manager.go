package engine

import (
	"context"
	"log"
	"sync"

	"github.com/torrentclaw/torrentclaw-cli/internal/agent"
)

// ManagerConfig holds download manager settings.
type ManagerConfig struct {
	MaxConcurrent int
	OutputDir     string
	Organize      OrganizeConfig
	Notifications bool // send desktop notifications on complete/fail
}

// Manager orchestrates concurrent downloads with method resolution and fallback.
type Manager struct {
	cfg         ManagerConfig
	reporter    *ProgressReporter
	downloaders map[DownloadMethod]Downloader

	activeMu sync.RWMutex
	active   map[string]*Task

	sem chan struct{}
	wg  sync.WaitGroup
}

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
		sem:         make(chan struct{}, cfg.MaxConcurrent),
	}
}

// Submit queues a task for download. Non-blocking if capacity available.
func (m *Manager) Submit(ctx context.Context, at agent.Task) {
	task := NewTaskFromAgent(at)

	m.activeMu.Lock()
	m.active[task.ID] = task
	m.activeMu.Unlock()

	m.reporter.Track(task)

	// Force start: bypass semaphore (like Transmission's "Force Start")
	if at.ForceStart {
		log.Printf("[%s] force start: bypassing queue", task.ID[:8])
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.processTask(ctx, task)
		}()
		return
	}

	// Acquire semaphore slot
	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() { <-m.sem }()
		m.processTask(ctx, task)
	}()
}

// HasCapacity returns true if there's room for more downloads.
func (m *Manager) HasCapacity() bool {
	return len(m.sem) < cap(m.sem)
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

// CancelTask cancels an active download by task ID (keeps partial files).
func (m *Manager) CancelTask(taskID string) {
	m.activeMu.RLock()
	task, ok := m.active[taskID]
	m.activeMu.RUnlock()

	if !ok {
		return
	}

	if dl, exists := m.downloaders[task.ResolvedMethod]; exists {
		dl.Pause(taskID) // stop download, keep files
	}

	task.mu.Lock()
	task.ErrorMessage = "cancelled by user"
	task.mu.Unlock()
	task.Transition(StatusCancelled)

	log.Printf("[%s] cancelled: %s", taskID[:8], task.Title)
}

// PauseTask pauses an active download (keeps partial files for resume).
func (m *Manager) PauseTask(taskID string) {
	m.activeMu.RLock()
	task, ok := m.active[taskID]
	m.activeMu.RUnlock()

	if !ok {
		return
	}

	if dl, exists := m.downloaders[task.ResolvedMethod]; exists {
		dl.Pause(taskID) // stop download, keep files for resume
	}

	task.Transition(StatusCancelled) // will be re-created as pending by server
	log.Printf("[%s] paused: %s", taskID[:8], task.Title)
}

// CancelAndDeleteFiles cancels a download and removes its files from disk.
func (m *Manager) CancelAndDeleteFiles(taskID string) {
	m.activeMu.RLock()
	task, ok := m.active[taskID]
	m.activeMu.RUnlock()

	if !ok {
		return
	}

	if dl, exists := m.downloaders[task.ResolvedMethod]; exists {
		dl.Cancel(taskID) // stop download + delete files
	}

	task.mu.Lock()
	task.ErrorMessage = "cancelled by user"
	task.mu.Unlock()
	task.Transition(StatusCancelled)

	log.Printf("[%s] cancelled + files deleted: %s", taskID[:8], task.Title)
}

// Wait blocks until all active downloads finish.
func (m *Manager) Wait() {
	m.wg.Wait()
}

// Shutdown stops accepting tasks and waits for active downloads to finish.
func (m *Manager) Shutdown(ctx context.Context) {
	// Wait for goroutines with timeout
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		log.Println("shutdown timeout, cancelling active downloads")
	}

	// Shutdown all downloaders
	for _, d := range m.downloaders {
		if err := d.Shutdown(ctx); err != nil {
			log.Printf("downloader shutdown: %v", err)
		}
	}

	// Clean active map
	m.activeMu.Lock()
	m.active = make(map[string]*Task)
	m.activeMu.Unlock()
}

func (m *Manager) processTask(ctx context.Context, task *Task) {
	defer func() {
		m.activeMu.Lock()
		delete(m.active, task.ID)
		m.activeMu.Unlock()
	}()

	// 1. Resolve method
	if err := task.Transition(StatusResolving); err != nil {
		m.fail(ctx, task, "transition error: "+err.Error())
		return
	}

	method, err := resolveMethod(ctx, task, m.downloaders)
	if err != nil {
		m.fail(ctx, task, "no method available: "+err.Error())
		return
	}

	task.ResolvedMethod = method
	log.Printf("[%s] resolved method: %s", task.ID[:8], method)

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
		// Try fallback
		if tryFallback(task, m.downloaders) {
			log.Printf("[%s] %s failed, trying fallback: %v", task.ID[:8], method, err)
			if err := task.Transition(StatusResolving); err == nil {
				m.processTaskRetry(ctx, task)
				return
			}
		}
		m.fail(ctx, task, err.Error())
		return
	}

	// 3. Verify
	if err := task.Transition(StatusVerifying); err != nil {
		m.fail(ctx, task, "transition error: "+err.Error())
		return
	}

	if err := verify(result); err != nil {
		m.fail(ctx, task, "verification failed: "+err.Error())
		return
	}

	// 4. Organize
	if err := task.Transition(StatusOrganizing); err != nil {
		m.fail(ctx, task, "transition error: "+err.Error())
		return
	}

	finalPath, err := organize(result, task, m.cfg.Organize)
	if err != nil {
		log.Printf("[%s] organize warning: %v (keeping in download dir)", task.ID[:8], err)
		finalPath = result.FilePath
	}

	task.mu.Lock()
	task.FilePath = finalPath
	task.mu.Unlock()

	// 4b. Handle upgrade replacement (mode = "upgrade")
	if task.ReplacePath != "" {
		backupDir := "" // uses default ~/.local/share/unarr/replaced/
		if err := replaceFile(task.ReplacePath, finalPath, backupDir); err != nil {
			log.Printf("[%s] replace warning: %v (keeping new file at %s)", task.ID[:8], err, finalPath)
		} else {
			task.mu.Lock()
			task.FilePath = task.ReplacePath
			task.mu.Unlock()
			log.Printf("[%s] upgraded: replaced %s", task.ID[:8], task.ReplacePath)
		}
	}

	// 5. Complete
	if method == MethodTorrent && m.cfg.Organize.Enabled {
		// Could add seeding here in the future
	}

	if err := task.Transition(StatusCompleted); err != nil {
		m.fail(ctx, task, "transition error: "+err.Error())
		return
	}

	log.Printf("[%s] completed: %s -> %s", task.ID[:8], task.Title, finalPath)
	if m.cfg.Notifications {
		desktopNotify("Download complete", task.Title)
	}
	m.reporter.ReportFinal(ctx, task)
}

// processTaskRetry handles fallback after a method failure.
func (m *Manager) processTaskRetry(ctx context.Context, task *Task) {
	method, err := resolveMethod(ctx, task, m.downloaders)
	if err != nil {
		m.fail(ctx, task, "fallback failed: "+err.Error())
		return
	}

	task.ResolvedMethod = method
	log.Printf("[%s] fallback to: %s", task.ID[:8], method)

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
		m.fail(ctx, task, err.Error())
		return
	}

	// Verify + Organize + Complete (same as processTask)
	task.Transition(StatusVerifying)
	if err := verify(result); err != nil {
		m.fail(ctx, task, "verification failed: "+err.Error())
		return
	}

	task.Transition(StatusOrganizing)
	finalPath, _ := organize(result, task, m.cfg.Organize)
	if finalPath == "" {
		finalPath = result.FilePath
	}
	task.mu.Lock()
	task.FilePath = finalPath
	task.mu.Unlock()

	task.Transition(StatusCompleted)
	log.Printf("[%s] completed (fallback): %s -> %s", task.ID[:8], task.Title, finalPath)
	m.reporter.ReportFinal(ctx, task)
}

func (m *Manager) fail(ctx context.Context, task *Task, msg string) {
	task.mu.Lock()
	task.ErrorMessage = msg
	task.mu.Unlock()
	task.Transition(StatusFailed)
	log.Printf("[%s] FAILED: %s — %s", task.ID[:8], task.Title, msg)
	if m.cfg.Notifications {
		desktopNotify("Download failed", task.Title+": "+msg)
	}
	m.reporter.ReportFinal(ctx, task)
}
