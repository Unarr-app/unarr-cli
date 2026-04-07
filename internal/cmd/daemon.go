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

Registers with the server, receives download tasks via WebSocket (with
HTTP fallback), and executes them using the configured download method.
Supports torrent, debrid, and usenet downloads concurrently.

The daemon sends periodic heartbeats and reports download progress back
to the web dashboard. Press Ctrl+C to stop gracefully — active downloads
get up to 30 seconds to finish.

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

	// Parse intervals
	pollInterval, _ := time.ParseDuration(cfg.Daemon.PollInterval)
	if pollInterval == 0 {
		pollInterval = 30 * time.Second
	}
	heartbeatInterval, _ := time.ParseDuration(cfg.Daemon.HeartbeatInterval)
	if heartbeatInterval == 0 {
		heartbeatInterval = 30 * time.Second
	}
	statusInterval, _ := time.ParseDuration(cfg.Daemon.StatusInterval)
	if statusInterval == 0 {
		statusInterval = 3 * time.Second
	}

	userAgent := "unarr/" + Version

	// Create daemon config
	daemonCfg := agent.DaemonConfig{
		AgentID:           cfg.Agent.ID,
		AgentName:         cfg.Agent.Name,
		Version:           Version,
		DownloadDir:       cfg.Download.Dir,
		PollInterval:      pollInterval,
		HeartbeatInterval: heartbeatInterval,
		StreamPort:        cfg.Download.StreamPort,
		LanIP:             engine.LanIP(),
		TailscaleIP:       engine.TailscaleIP(),
	}

	// Create transport: Hybrid (WS + HTTP fallback) or HTTP-only
	httpT := agent.NewHTTPTransport(cfg.Auth.APIURL, cfg.Auth.APIKey, userAgent)

	wsURL := cfg.Auth.WSURL
	if wsURL == "" {
		wsURL = deriveWSURL(cfg.Auth.APIURL, cfg.Agent.ID)
	}

	var transport agent.Transport
	if wsURL != "" {
		wsT := agent.NewWSTransport(wsURL, cfg.Auth.APIKey, cfg.Agent.ID, userAgent)
		transport = agent.NewHybridTransport(wsT, httpT)
		log.Printf("Transport: WebSocket (fallback: HTTP) → %s", wsURL)
	} else {
		transport = httpT
		log.Println("Transport: HTTP only")
	}

	// Create daemon — always uses Transport interface
	d := agent.NewDaemon(daemonCfg, transport)

	// Create agent client for watch progress reporting
	agentClient := agent.NewClient(cfg.Auth.APIURL, cfg.Auth.APIKey, userAgent)

	// Daemon-scoped context — cancelled on shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create progress reporter using transport
	reporter := engine.NewProgressReporterWithTransport(transport, statusInterval)
	reporter.SetWatchingFunc(func() bool { return d.Watching.Load() })
	reporter.SetWatchingChangedHandler(func(watching bool) { d.Watching.Store(watching) })

	// Parse speed limits
	maxDl, _ := config.ParseSpeed(cfg.Download.MaxDownloadSpeed)
	maxUl, _ := config.ParseSpeed(cfg.Download.MaxUploadSpeed)

	// Parse torrent timeouts from config (default: 0 = unlimited, like qBittorrent)
	metaTimeout, _ := time.ParseDuration(cfg.Download.MetadataTimeout)
	stallTimeout, _ := time.ParseDuration(cfg.Download.StallTimeout)

	// Create torrent downloader
	torrentDl, err := engine.NewTorrentDownloader(engine.TorrentConfig{
		DataDir:         cfg.Download.Dir,
		MetadataTimeout: metaTimeout,  // 0 = unlimited (default)
		StallTimeout:    stallTimeout, // 0 = unlimited (default)
		MaxTimeout:      0,            // unlimited
		MaxDownloadRate: maxDl,
		MaxUploadRate:   maxUl,
		ListenPort:      cfg.Download.ListenPort, // 0 = default 42069
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

	// Create debrid downloader (HTTPS-based, no provider interaction needed)
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
	}, reporter, torrentDl, debridDl, engine.NewUsenetDownloader(httpT.Client()))

	// Create persistent stream server — lives for the entire daemon lifecycle.
	// One port, one server, swap files with SetFile(). No more port churn.
	streamSrv := engine.NewStreamServer(cfg.Download.StreamPort)
	if err := streamSrv.Listen(ctx); err != nil {
		return fmt.Errorf("start stream server: %w", err)
	}
	// Update heartbeat with actual port (may differ if configured port was busy)
	d.UpdateStreamPort(streamSrv.Port())

	// Wire state tracking
	d.GetActiveCount = manager.ActiveCount
	d.GetCleanableBytes = CleanableBytes

	// Wire: server-side signals -> manager actions + stream tasks
	reporter.SetCancelHandler(func(taskID string) {
		manager.CancelTask(taskID)
		cancelStreamTask(taskID)
	})
	reporter.SetPauseHandler(func(taskID string) {
		manager.PauseTask(taskID)
		cancelStreamTask(taskID)
	})
	reporter.SetDeleteFilesHandler(func(taskID string) {
		manager.CancelAndDeleteFiles(taskID)
		cancelStreamTask(taskID)
	})

	// Wire: stream requested on active download → set file on persistent server
	reporter.SetStreamRequestedHandler(func(taskID string) {
		task := manager.GetTask(taskID)
		if task == nil {
			log.Printf("[%s] stream requested but task not found in manager", taskID[:8])
			return
		}
		if task.GetStreamURL() != "" {
			return // already streaming
		}
		provider, err := torrentDl.GetStreamProvider(taskID)
		if err != nil {
			log.Printf("[%s] stream failed: %v", taskID[:8], err)
			return
		}
		cancelStreamContexts()
		streamSrv.SetFile(provider, taskID)
		task.SetStreamURL(streamSrv.URLsJSON())
		log.Printf("[%s] streaming active download: %s", taskID[:8], provider.FileName())

		// Start watch progress reporter with cancellable context
		watchCtx, watchCancel := context.WithCancel(ctx) //nolint:gosec // cancel stored in streamRegistry, called by cancelStreamContexts()
		streamRegistry.mu.Lock()
		streamRegistry.cancels["watch:"+taskID] = watchCancel
		streamRegistry.mu.Unlock()
		go engine.NewWatchReporter(agentClient, streamSrv, taskID).Run(watchCtx)
	})

	// Wire: daemon claimed tasks -> manager

	d.OnTasksClaimed = func(tasks []agent.Task) {
		for _, t := range tasks {
			if t.Mode == "stream" {
				// Skip if already streaming this task
				if isStreamingTask(t.ID) {
					continue
				}
				// Only 1 stream at a time: cancel existing stream goroutines + clear file
				cancelStreamContexts()
				streamSrv.ClearFile()
				// Reserve slot before spawning goroutine to prevent TOCTOU race.
				streamCtx, streamCancel := context.WithCancel(ctx) //nolint:gosec // G118: cancel ownership transferred to streamRegistry
				streamRegistry.mu.Lock()
				streamRegistry.cancels[t.ID] = streamCancel
				streamRegistry.mu.Unlock()
				go handleStreamTask(streamCtx, t, reporter, cfg, agentClient, streamSrv)
			} else if t.ForceStart || manager.HasCapacity() {
				manager.Submit(ctx, t)
			} else {
				log.Printf("[%s] skipped: no capacity (max %d)", t.ID[:8], cfg.Download.MaxConcurrent)
			}
		}
	}

	// Wire: stream requests for completed downloads → set file on persistent server
	d.OnStreamRequested = func(sr agent.StreamRequest) {
		// Already serving this task — just notify server it's ready
		if streamSrv.CurrentTaskID() == sr.TaskID {
			go func() {
				if _, err := transport.SendProgress(ctx, agent.StatusUpdate{
					TaskID:      sr.TaskID,
					StreamReady: true,
				}); err != nil {
					log.Printf("[%s] stream ready re-notify failed: %v", sr.TaskID[:8], err)
				}
			}()
			return
		}

		filePath := sr.FilePath
		info, err := os.Stat(filePath)
		if err != nil {
			log.Printf("[%s] stream request: file not found: %s", sr.TaskID[:8], filePath)
			go func() {
				if _, err := transport.SendProgress(ctx, agent.StatusUpdate{
					TaskID:       sr.TaskID,
					Status:       "failed",
					ErrorMessage: fmt.Sprintf("file not found: %s", filePath),
				}); err != nil {
					log.Printf("[%s] stream error report failed: %v", sr.TaskID[:8], err)
				}
			}()
			return
		}

		// If filePath is a directory, find the largest video file inside
		if info.IsDir() {
			found := engine.FindVideoFile(filePath)
			if found == "" {
				log.Printf("[%s] stream request: no video file in directory: %s", sr.TaskID[:8], filePath)
				go func() {
					if _, err := transport.SendProgress(ctx, agent.StatusUpdate{
						TaskID:       sr.TaskID,
						Status:       "failed",
						ErrorMessage: fmt.Sprintf("no video file in directory: %s", filePath),
					}); err != nil {
						log.Printf("[%s] stream error report failed: %v", sr.TaskID[:8], err)
					}
				}()
				return
			}
			filePath = found
			log.Printf("[%s] resolved directory to video file: %s", sr.TaskID[:8], filepath.Base(filePath))
		}

		// Cancel any active stream goroutines and swap file on the persistent server
		cancelStreamContexts()
		streamSrv.SetFile(engine.NewDiskFileProvider(filePath), sr.TaskID)

		log.Printf("[%s] streaming from disk: %s → %s", sr.TaskID[:8], filepath.Base(filePath), streamSrv.URL())

		// Start watch progress reporter with a cancellable context
		// so it stops when the user switches to a different stream.
		watchCtx, watchCancel := context.WithCancel(ctx) //nolint:gosec // cancel stored in streamRegistry, called by cancelStreamContexts()
		streamRegistry.mu.Lock()
		streamRegistry.cancels["watch:"+sr.TaskID] = watchCancel
		streamRegistry.mu.Unlock()
		go engine.NewWatchReporter(agentClient, streamSrv, sr.TaskID).Run(watchCtx)

		// Notify server that stream is ready (clears streamRequested flag)
		go func() {
			if _, err := transport.SendProgress(ctx, agent.StatusUpdate{
				TaskID:      sr.TaskID,
				StreamReady: true,
			}); err != nil {
				log.Printf("[%s] stream ready report failed: %v", sr.TaskID[:8], err)
			}
		}()
	}

	// Wire: WS control actions (pause/cancel/stream pushed from server)
	d.OnControlAction = func(action, taskID string) {
		switch action {
		case "cancel":
			manager.CancelTask(taskID)
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
			log.Printf("[%s] resume requested via WebSocket, triggering poll", taskID[:8])
			d.TriggerPoll()
		case "stream":
			// Skip if already streaming this task
			if streamSrv.CurrentTaskID() == taskID {
				return
			}
			task := manager.GetTask(taskID)
			if task == nil || task.GetStreamURL() != "" {
				return
			}
			provider, err := torrentDl.GetStreamProvider(taskID)
			if err != nil {
				log.Printf("[%s] stream failed: %v", taskID[:8], err)
				return
			}
			cancelStreamContexts()
			streamSrv.SetFile(provider, taskID)
			task.SetStreamURL(streamSrv.URLsJSON())
			log.Printf("[%s] streaming via WS: %s", taskID[:8], provider.FileName())
		case "stop-stream":
			cancelStreamTask(taskID)
			if streamSrv.CurrentTaskID() == taskID {
				streamSrv.ClearFile()
			}
		}
	}

	// Config hot-reload (SIGUSR1 on Unix, no-op on Windows)
	// Tickers are initialized inside d.Run(), so we pass the daemon
	// and the reload goroutine reads them when the signal arrives.
	startReloadWatcher(&ReloadableConfig{Daemon: d})

	// Signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start progress reporter in background
	go reporter.Run(ctx)

	// Periodic DHT node persistence (every 5 min) — protects against crash data loss
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

	// Start auto-scan goroutine (daily library scan + sync)
	// Default scan_path to download dir so auto-scan works out of the box.
	scanPath := cfg.Library.ScanPath
	if scanPath == "" {
		scanPath = cfg.Download.Dir
	}
	if scanPath != "" && cfg.Library.AutoScan {
		scanCfg := cfg
		scanCfg.Library.ScanPath = scanPath
		scanInterval := 24 * time.Hour
		if cfg.Library.ScanInterval != "" {
			if parsed, err := time.ParseDuration(cfg.Library.ScanInterval); err == nil && parsed > 0 {
				scanInterval = parsed
			}
		}
		go runAutoScan(ctx, scanCfg, scanInterval, agentClient, d.ScanNow)
	}

	// Start daemon (blocks)
	errCh := make(chan error, 1)
	go func() {
		errCh <- d.Run(ctx)
	}()

	// Start idle guard for the persistent stream server
	go startIdleGuard(ctx, streamSrv)

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

		fmt.Println("  Daemon stopped.")
		return nil

	case err := <-errCh:
		cancelStreamContexts()
		streamSrv.Shutdown(context.Background())
		cancel()
		return err
	}
}

// deriveWSURL derives a WebSocket URL from the API URL.
// https://torrentclaw.com → wss://unarr.torrentclaw.com/ws/{agentId}
// Returns "" for localhost/dev environments where WS gateway isn't available.
func deriveWSURL(apiURL, agentID string) string {
	if apiURL == "" || agentID == "" {
		return ""
	}
	// Parse domain from API URL
	domain := apiURL
	for _, prefix := range []string{"https://", "http://"} {
		if len(domain) > len(prefix) && domain[:len(prefix)] == prefix {
			domain = domain[len(prefix):]
			break
		}
	}
	// Strip trailing slash/path
	for i := 0; i < len(domain); i++ {
		if domain[i] == '/' {
			domain = domain[:i]
			break
		}
	}
	// Strip port if present
	if idx := strings.LastIndex(domain, ":"); idx > 0 {
		domain = domain[:idx]
	}

	// Skip WS for localhost/dev — gateway only available in production
	if domain == "localhost" || domain == "127.0.0.1" || domain == "0.0.0.0" {
		return ""
	}

	return "wss://unarr." + domain + "/ws/" + agentID
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
func runAutoScan(ctx context.Context, cfg config.Config, interval time.Duration, ac *agent.Client, scanNow <-chan struct{}) {
	log.Printf("[auto-scan] enabled: every %s, path: %s", interval, cfg.Library.ScanPath)

	// Run first scan after a short delay (let daemon stabilize)
	select {
	case <-time.After(30 * time.Second):
	case <-scanNow:
		// Immediate scan requested before initial delay
	case <-ctx.Done():
		return
	}

	doScan := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[auto-scan] panic recovered: %v", r)
			}
		}()
		log.Printf("[auto-scan] starting scan of %s", cfg.Library.ScanPath)

		existing, _ := library.LoadCache()

		workers := cfg.Library.Workers
		if workers == 0 {
			workers = 8
		}

		cache, err := library.Scan(ctx, cfg.Library.ScanPath, existing, library.ScanOptions{
			Workers:     workers,
			FFprobePath: cfg.Library.FFprobePath,
			Incremental: existing != nil,
		})
		if err != nil {
			log.Printf("[auto-scan] scan failed: %v", err)
			return
		}

		if err := library.SaveCache(cache); err != nil {
			log.Printf("[auto-scan] save cache failed: %v", err)
			return
		}

		// Sync to server
		items := library.BuildSyncItems(cache)
		if len(items) == 0 {
			log.Printf("[auto-scan] no items to sync")
			return
		}

		const batchSize = 100
		syncStartedAt := time.Now().UTC().Format(time.RFC3339)
		for i := 0; i < len(items); i += batchSize {
			end := i + batchSize
			if end > len(items) {
				end = len(items)
			}
			isLast := end >= len(items)

			_, err := ac.SyncLibrary(ctx, agent.LibrarySyncRequest{
				Items:         items[i:end],
				ScanPath:      cache.Path,
				IsLastBatch:   isLast,
				SyncStartedAt: syncStartedAt,
			})
			if err != nil {
				log.Printf("[auto-scan] sync failed: %v", err)
				return
			}
		}

		log.Printf("[auto-scan] synced %d items", len(items))
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
