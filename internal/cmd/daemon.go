package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/config"
	"github.com/torrentclaw/unarr/internal/engine"
	"github.com/torrentclaw/unarr/internal/library"
	"github.com/torrentclaw/unarr/internal/usenet/download"
)

// newStartCmd creates the top-level `unarr start` command.
func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the download daemon (foreground)",
		Long: `Start the unarr daemon in the foreground.

Registers with the server, receives download tasks via periodic sync,
and executes them using the configured download method.
Supports torrent, debrid, and usenet downloads concurrently.

The daemon syncs state with the server every 3s when someone is viewing
the web dashboard, or every 60s when idle. Press Ctrl+C to stop
gracefully — active downloads get up to 30 seconds to finish.

Requires: API key, agent ID, and download directory (run 'unarr init' first).

To run as a background service, use 'unarr daemon install' instead.`,
		Example: `  unarr start
  unarr start --config /path/to/config.toml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonStart()
		},
	}
}

// newStopCmd creates the top-level `unarr stop` placeholder.
func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		Long: `Stop the unarr daemon.

If running in the foreground, press Ctrl+C in the terminal where it was started.
If installed as a system service, use your OS service manager:

  Linux (systemd):   systemctl --user stop unarr
  macOS (launchd):   launchctl unload ~/Library/LaunchAgents/com.torrentclaw.unarr.plist`,
		Example: `  unarr stop`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("  Use Ctrl+C in the terminal where the daemon is running.")
			fmt.Println()
			fmt.Println("  If installed as a service:")
			fmt.Println("    Linux:  systemctl --user stop unarr")
			fmt.Println("    macOS:  launchctl unload ~/Library/LaunchAgents/com.torrentclaw.unarr.plist")
			fmt.Println()
			return nil
		},
	}
}

// newDaemonCmd creates `unarr daemon` for administrative subcommands.
func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon <command>",
		Short: "Manage the daemon as a system service",
		Long: `Install or remove unarr as a system service that starts automatically on boot.

  Linux:  Creates a systemd user service (~/.config/systemd/user/unarr.service)
  macOS:  Creates a launchd agent (~/Library/LaunchAgents/com.torrentclaw.unarr.plist)`,
		Example: `  unarr daemon install
  unarr daemon uninstall`,
	}

	cmd.AddCommand(
		newDaemonInstallCmdReal(),
		newDaemonUninstallCmdReal(),
	)

	return cmd
}

func runDaemonStart() error {
	cfg := loadConfig()
	bold := color.New(color.Bold)

	// Validate config
	if cfg.Auth.APIKey == "" {
		return fmt.Errorf("no API key configured — run 'unarr init' first")
	}
	if cfg.Agent.ID == "" {
		return fmt.Errorf("no agent ID — run 'unarr init' first")
	}
	if cfg.Download.Dir == "" {
		return fmt.Errorf("no download directory — run 'unarr init' first")
	}

	// Validate configured paths are safe
	if err := cfg.ValidatePaths(); err != nil {
		return fmt.Errorf("unsafe configuration: %w", err)
	}

	// Ensure download dir exists
	if err := os.MkdirAll(cfg.Download.Dir, 0o755); err != nil {
		return fmt.Errorf("create download dir: %w", err)
	}

	// Clean up stale resume files (>7 days old)
	resumeDir := filepath.Join(config.DataDir(), "resume")
	if removed := download.CleanStaleFiles(resumeDir, 7*24*time.Hour); removed > 0 {
		log.Printf("Cleaned %d stale resume file(s)", removed)
	}

	fmt.Println()
	bold.Println("  unarr Daemon")
	fmt.Println()

	userAgent := "unarr/" + Version

	// Create daemon config
	daemonCfg := agent.DaemonConfig{
		AgentID:     cfg.Agent.ID,
		AgentName:   cfg.Agent.Name,
		Version:     Version,
		DownloadDir: cfg.Download.Dir,
		StreamPort:  cfg.Download.StreamPort,
		LanIP:       engine.LanIP(),
		TailscaleIP: engine.TailscaleIP(),
		CanDelete:   cfg.Library.AllowDelete,
		ScanPaths:   library.ResolveScanPaths(cfg.Download.Dir, cfg.Organize.MoviesDir, cfg.Organize.TVShowsDir, cfg.Library.ScanPath),
	}

	// Create HTTP client — single communication channel
	agentClient := agent.NewClient(cfg.Auth.APIURL, cfg.Auth.APIKey, userAgent)
	log.Printf("Transport: HTTP sync → %s", cfg.Auth.APIURL)

	// Create daemon
	d := agent.NewDaemon(daemonCfg, agentClient)

	// Start SIGUSR1 reload watcher (unix only, no-op on Windows)
	startReloadWatcher(&ReloadableConfig{Daemon: d})

	// Daemon-scoped context — cancelled on shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Parse speed limits
	maxDl, _ := config.ParseSpeed(cfg.Download.MaxDownloadSpeed)
	maxUl, _ := config.ParseSpeed(cfg.Download.MaxUploadSpeed)

	// Parse torrent timeouts
	metaTimeout, _ := time.ParseDuration(cfg.Download.MetadataTimeout)
	stallTimeout, _ := time.ParseDuration(cfg.Download.StallTimeout)

	// Create progress reporter — only used for stream tasks (handleStreamTask)
	// The sync goroutine handles all regular progress reporting.
	statusInterval, _ := time.ParseDuration(cfg.Daemon.StatusInterval)
	if statusInterval == 0 {
		statusInterval = 3 * time.Second
	}
	reporter := engine.NewProgressReporter(agentClient, statusInterval)
	reporter.SetWatchingFunc(func() bool { return d.Watching.Load() })

	// Create torrent downloader
	torrentDl, err := engine.NewTorrentDownloader(engine.TorrentConfig{
		DataDir:         cfg.Download.Dir,
		MetadataTimeout: metaTimeout,
		StallTimeout:    stallTimeout,
		MaxTimeout:      0,
		MaxDownloadRate: maxDl,
		MaxUploadRate:   maxUl,
		ListenPort:      cfg.Download.ListenPort,
		SeedEnabled:     false,
	})
	if err != nil {
		return fmt.Errorf("create torrent downloader: %w", err)
	}

	if maxDl > 0 || maxUl > 0 {
		dlStr, ulStr := "unlimited", "unlimited"
		if maxDl > 0 {
			dlStr = formatSpeedLog(maxDl)
		}
		if maxUl > 0 {
			ulStr = formatSpeedLog(maxUl)
		}
		log.Printf("Speed limits: download=%s upload=%s", dlStr, ulStr)
	}

	// Create debrid downloader
	debridDl := engine.NewDebridDownloader()

	// Create download manager
	manager := engine.NewManager(engine.ManagerConfig{
		MaxConcurrent: cfg.Download.MaxConcurrent,
		OutputDir:     cfg.Download.Dir,
		Notifications: cfg.Notifications.Enabled,
		Organize: engine.OrganizeConfig{
			Enabled:    cfg.Organize.Enabled,
			MoviesDir:  cfg.Organize.MoviesDir,
			TVShowsDir: cfg.Organize.TVShowsDir,
			OutputDir:  cfg.Download.Dir,
		},
	}, reporter, torrentDl, debridDl, engine.NewUsenetDownloader(agentClient))

	// Create persistent stream server
	streamSrv := engine.NewStreamServer(cfg.Download.StreamPort)
	if err := streamSrv.Listen(ctx); err != nil {
		return fmt.Errorf("start stream server: %w", err)
	}
	d.UpdateStreamPort(streamSrv.Port())

	// Wire sync client callbacks
	sc := d.SyncClient()
	sc.GetFreeSlots = manager.FreeSlots
	sc.GetTaskStates = manager.TaskStates
	d.GetActiveCount = manager.ActiveCount

	// Trigger immediate sync when a download slot frees up
	manager.OnTaskDone = func() { d.TriggerSync() }

	// Wire: sync receives new tasks → submit to manager or handle stream
	d.OnTasksClaimed = func(tasks []agent.Task) {
		for _, t := range tasks {
			if t.Mode == "stream" {
				if isStreamingTask(t.ID) {
					continue
				}
				cancelStreamContexts()
				streamSrv.ClearFile()
				streamCtx, streamCancel := context.WithCancel(ctx) //nolint:gosec // G118: cancel stored in registry
				streamRegistry.mu.Lock()
				streamRegistry.cancels[t.ID] = streamCancel
				streamRegistry.mu.Unlock()
				go handleStreamTask(streamCtx, t, reporter, cfg, agentClient, streamSrv)
			} else {
				manager.Submit(ctx, t)
			}
		}
	}

	// Wire: sync receives control signals → act on manager
	d.OnControlAction = func(action, taskID string, deleteFiles bool) {
		switch action {
		case "cancel":
			if deleteFiles {
				manager.CancelAndDeleteFiles(taskID)
			} else {
				manager.CancelTask(taskID)
			}
			cancelStreamTask(taskID)
			if streamSrv.CurrentTaskID() == taskID {
				streamSrv.ClearFile()
			}
		case "pause":
			manager.PauseTask(taskID)
			cancelStreamTask(taskID)
			if streamSrv.CurrentTaskID() == taskID {
				streamSrv.ClearFile()
			}
		case "resume":
			log.Printf("[%s] resume requested, triggering sync", agent.ShortID(taskID))
			d.TriggerSync()
		case "stream":
			if streamSrv.CurrentTaskID() == taskID {
				return
			}
			task := manager.GetTask(taskID)
			if task == nil || task.GetStreamURL() != "" {
				return
			}
			provider, err := torrentDl.GetStreamProvider(taskID)
			if err != nil {
				log.Printf("[%s] stream failed: %v", agent.ShortID(taskID), err)
				return
			}
			cancelStreamContexts()
			streamSrv.SetFile(provider, taskID)
			task.SetStreamURL(streamSrv.URLsJSON())
			log.Printf("[%s] streaming: %s", agent.ShortID(taskID), provider.FileName())

			watchCtx, watchCancel := context.WithCancel(ctx) //nolint:gosec // G118
			streamRegistry.mu.Lock()
			streamRegistry.cancels["watch:"+taskID] = watchCancel
			streamRegistry.mu.Unlock()
			go engine.NewWatchReporter(agentClient, streamSrv, taskID).Run(watchCtx)
		case "stop-stream":
			cancelStreamTask(taskID)
			if streamSrv.CurrentTaskID() == taskID {
				streamSrv.ClearFile()
			}
		}
	}

	// Wire: sync receives file deletion requests from the server
	if cfg.Library.AllowDelete && len(daemonCfg.ScanPaths) > 0 {
		sc.OnDeleteFiles = func(items []agent.LibraryDeleteRequest) []int {
			return library.DeleteFiles(items, daemonCfg.ScanPaths)
		}
	}

	// Wire: sync receives stream requests for completed downloads
	d.OnStreamRequested = func(sr agent.StreamRequest) {
		if streamSrv.CurrentTaskID() == sr.TaskID {
			// Already serving — notify server it's ready
			go func() {
				if _, err := agentClient.ReportStatus(ctx, agent.StatusUpdate{
					TaskID:      sr.TaskID,
					StreamReady: true,
				}); err != nil {
					log.Printf("[%s] stream ready re-notify failed: %v", agent.ShortID(sr.TaskID), err)
				}
			}()
			return
		}

		filePath := filepath.Clean(sr.FilePath)
		if !isAllowedStreamPath(filePath, cfg.Download.Dir, cfg.Library.ScanPath,
			cfg.Organize.MoviesDir, cfg.Organize.TVShowsDir) {
			log.Printf("[%s] stream request rejected: path outside allowed dirs: %s", agent.ShortID(sr.TaskID), filePath)
			go func() {
				if _, err := agentClient.ReportStatus(ctx, agent.StatusUpdate{
					TaskID:       sr.TaskID,
					Status:       "failed",
					ErrorMessage: fmt.Sprintf("path outside allowed dirs: %s", filePath),
				}); err != nil {
					log.Printf("[%s] stream error report failed: %v", agent.ShortID(sr.TaskID), err)
				}
			}()
			return
		}
		info, err := os.Stat(filePath)
		if err != nil {
			log.Printf("[%s] stream request: file not found: %s", agent.ShortID(sr.TaskID), filePath)
			go func() {
				if _, err := agentClient.ReportStatus(ctx, agent.StatusUpdate{
					TaskID:       sr.TaskID,
					Status:       "failed",
					ErrorMessage: fmt.Sprintf("file not found: %s", filePath),
				}); err != nil {
					log.Printf("[%s] stream error report failed: %v", agent.ShortID(sr.TaskID), err)
				}
			}()
			return
		}

		if info.IsDir() {
			found := engine.FindVideoFile(filePath)
			if found == "" {
				log.Printf("[%s] stream request: no video file in directory: %s", agent.ShortID(sr.TaskID), filePath)
				go func() {
					if _, err := agentClient.ReportStatus(ctx, agent.StatusUpdate{
						TaskID:       sr.TaskID,
						Status:       "failed",
						ErrorMessage: fmt.Sprintf("no video file in directory: %s", filePath),
					}); err != nil {
						log.Printf("[%s] stream error report failed: %v", agent.ShortID(sr.TaskID), err)
					}
				}()
				return
			}
			filePath = found
			log.Printf("[%s] resolved directory to video file: %s", agent.ShortID(sr.TaskID), filepath.Base(filePath))
		}

		cancelStreamContexts()
		streamSrv.SetFile(engine.NewDiskFileProvider(filePath), sr.TaskID)
		log.Printf("[%s] streaming from disk: %s → %s", agent.ShortID(sr.TaskID), filepath.Base(filePath), streamSrv.URL())

		watchCtx, watchCancel := context.WithCancel(ctx) //nolint:gosec // G118
		streamRegistry.mu.Lock()
		streamRegistry.cancels["watch:"+sr.TaskID] = watchCancel
		streamRegistry.mu.Unlock()
		go engine.NewWatchReporter(agentClient, streamSrv, sr.TaskID).Run(watchCtx)

		go func() {
			if _, err := agentClient.ReportStatus(ctx, agent.StatusUpdate{
				TaskID:      sr.TaskID,
				StreamReady: true,
			}); err != nil {
				log.Printf("[%s] stream ready report failed: %v", agent.ShortID(sr.TaskID), err)
			}
		}()
	}

	// Periodic DHT node persistence (every 5 min)
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				torrentDl.SaveDhtNodes()
			case <-ctx.Done():
				return
			}
		}
	}()

	// Start auto-scan goroutine
	scanPaths := daemonCfg.ScanPaths
	if len(scanPaths) > 0 && cfg.Library.AutoScan {
		scanInterval := 24 * time.Hour
		if cfg.Library.ScanInterval != "" {
			if parsed, err := time.ParseDuration(cfg.Library.ScanInterval); err == nil && parsed > 0 {
				scanInterval = parsed
			}
		}
		go runAutoScan(ctx, cfg, scanInterval, agentClient, d.ScanNow, scanPaths)
	}

	// Start reporter only for stream task handling
	go reporter.Run(ctx)

	// Start daemon (blocks — runs sync loop)
	errCh := make(chan error, 1)
	go func() {
		errCh <- d.Run(ctx)
	}()

	// Start idle guard for the persistent stream server
	go startIdleGuard(ctx, streamSrv)

	// Signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Wait for signal or error
	select {
	case sig := <-sigCh:
		fmt.Printf("\n  Received %s, shutting down...\n", sig)
		cancelStreamContexts()
		streamSrv.Shutdown(context.Background())
		cancel()

		// Give active downloads 30s to finish
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		manager.Shutdown(shutdownCtx)

		d.Deregister()
		fmt.Println("  Daemon stopped.")
		return nil

	case err := <-errCh:
		cancelStreamContexts()
		streamSrv.Shutdown(context.Background())
		cancel()
		return err
	}
}

// isAllowedStreamPath checks that filePath is within one of the directories
// the daemon is configured to manage. This defends against a compromised API
// server sending a path traversal payload (e.g. /etc/passwd) in StreamRequest.
// isAllowedStreamPath reports whether filePath is contained within one of the
// allowedDirs. filePath must already be cleaned (filepath.Clean) by the caller.
// This defends against a compromised API server sending a path traversal payload.
func isAllowedStreamPath(filePath string, allowedDirs ...string) bool {
	for _, dir := range allowedDirs {
		if dir == "" {
			continue
		}
		rel, err := filepath.Rel(filepath.Clean(dir), filePath)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	return false
}

func formatSpeedLog(bps int64) string {
	switch {
	case bps >= 1024*1024*1024:
		return fmt.Sprintf("%.1f GB/s", float64(bps)/(1024*1024*1024))
	case bps >= 1024*1024:
		return fmt.Sprintf("%.1f MB/s", float64(bps)/(1024*1024))
	case bps >= 1024:
		return fmt.Sprintf("%.0f KB/s", float64(bps)/1024)
	default:
		return fmt.Sprintf("%d B/s", bps)
	}
}

// runAutoScan runs a library scan + sync on a timer or on-demand via scanNow channel.
// It scans all provided paths and syncs each independently so stale-item cleanup
// is scoped to the correct directory prefix on the server.
func runAutoScan(ctx context.Context, cfg config.Config, interval time.Duration, ac *agent.Client, scanNow <-chan struct{}, scanPaths []string) {
	log.Printf("[auto-scan] enabled: every %s, paths: %v", interval, scanPaths)

	select {
	case <-time.After(30 * time.Second):
	case <-scanNow:
	case <-ctx.Done():
		return
	}

	doScan := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[auto-scan] panic recovered: %v", r)
			}
		}()
		log.Printf("[auto-scan] starting scan of %v", scanPaths)

		existing, _ := library.LoadCache()

		workers := cfg.Library.Workers
		if workers == 0 {
			workers = 8
		}

		scanOpts := library.ScanOptions{
			Workers:     workers,
			FFprobePath: cfg.Library.FFprobePath,
			Incremental: existing != nil,
		}

		// Scan each path independently and sync per path so the server can
		// scope stale-item deletion to the correct directory prefix.
		const batchSize = 100
		totalSynced := 0
		var mergedItems []library.LibraryItem

		for _, scanPath := range scanPaths {
			cache, err := library.Scan(ctx, scanPath, existing, scanOpts)
			if err != nil {
				log.Printf("[auto-scan] scan failed for %s: %v", scanPath, err)
				continue
			}
			mergedItems = append(mergedItems, cache.Items...)

			items := library.BuildSyncItems(cache)
			if len(items) == 0 {
				log.Printf("[auto-scan] no items under %s", scanPath)
				continue
			}

			syncStartedAt := time.Now().UTC().Format(time.RFC3339)
			for i := 0; i < len(items); i += batchSize {
				end := i + batchSize
				if end > len(items) {
					end = len(items)
				}
				isLast := end >= len(items)

				_, err := ac.SyncLibrary(ctx, agent.LibrarySyncRequest{
					Items:         items[i:end],
					ScanPath:      scanPath,
					IsLastBatch:   isLast,
					SyncStartedAt: syncStartedAt,
				})
				if err != nil {
					log.Printf("[auto-scan] sync failed for %s: %v", scanPath, err)
					break
				}
			}
			totalSynced += len(items)
		}

		// Save merged cache for incremental scanning next time.
		if len(mergedItems) > 0 {
			mergedCache := &library.LibraryCache{
				ScannedAt: time.Now().UTC().Format(time.RFC3339),
				Path:      scanPaths[0],
				Items:     mergedItems,
			}
			if err := library.SaveCache(mergedCache); err != nil {
				log.Printf("[auto-scan] save cache failed: %v", err)
			}
		}

		log.Printf("[auto-scan] synced %d items across %d path(s)", totalSynced, len(scanPaths))
	}

	doScan()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			doScan()
		case <-scanNow:
			log.Printf("[auto-scan] on-demand scan triggered")
			ticker.Reset(interval)
			doScan()
		case <-ctx.Done():
			return
		}
	}
}
