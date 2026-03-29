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
	"github.com/torrentclaw/torrentclaw-cli/internal/agent"
	"github.com/torrentclaw/torrentclaw-cli/internal/config"
	"github.com/torrentclaw/torrentclaw-cli/internal/engine"
	"github.com/torrentclaw/torrentclaw-cli/internal/usenet/download"
	"github.com/torrentclaw/torrentclaw-cli/internal/upgrade"
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

	userAgent := "unarr/" + Version

	// Create daemon config
	daemonCfg := agent.DaemonConfig{
		AgentID:           cfg.Agent.ID,
		AgentName:         cfg.Agent.Name,
		Version:           Version,
		DownloadDir:       cfg.Download.Dir,
		PollInterval:      pollInterval,
		HeartbeatInterval: heartbeatInterval,
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

	// Create progress reporter using transport
	reporter := engine.NewProgressReporterWithTransport(transport, 3*time.Second)

	// Parse speed limits
	maxDl, _ := config.ParseSpeed(cfg.Download.MaxDownloadSpeed)
	maxUl, _ := config.ParseSpeed(cfg.Download.MaxUploadSpeed)

	// Create torrent downloader
	torrentDl, err := engine.NewTorrentDownloader(engine.TorrentConfig{
		DataDir:         cfg.Download.Dir,
		StallTimeout:    90 * time.Second,
		MaxTimeout:      30 * time.Minute,
		MaxDownloadRate: maxDl,
		MaxUploadRate:   maxUl,
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
		},
	}, reporter, torrentDl, debridDl, engine.NewUsenetDownloader(httpT.Client()))

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

	// Wire: stream requested on active download → start HTTP server
	reporter.SetStreamRequestedHandler(func(taskID string) {
		task := manager.GetTask(taskID)
		if task == nil {
			log.Printf("[%s] stream requested but task not found in manager", taskID[:8])
			return
		}
		if task.GetStreamURL() != "" {
			return // already streaming
		}
		srv, err := torrentDl.StartStream(taskID)
		if err != nil {
			log.Printf("[%s] stream failed: %v", taskID[:8], err)
			return
		}
		// Register server before setting URL to avoid TOCTOU race
		streamRegistry.mu.Lock()
		streamRegistry.servers[taskID] = srv
		streamRegistry.mu.Unlock()
		task.SetStreamURL(srv.URL())
	})

	// Wire: daemon claimed tasks -> manager
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.OnTasksClaimed = func(tasks []agent.Task) {
		for _, t := range tasks {
			if t.Mode == "stream" {
				go handleStreamTask(ctx, t, reporter, cfg)
			} else if manager.HasCapacity() {
				manager.Submit(ctx, t)
			} else {
				log.Printf("[%s] skipped: no capacity (max %d)", t.ID[:8], cfg.Download.MaxConcurrent)
			}
		}
	}

	// Wire: stream requests for completed downloads → serve file from disk
	d.OnStreamRequested = func(sr agent.StreamRequest) {
		// Check if already streaming this task
		streamRegistry.mu.Lock()
		_, exists := streamRegistry.servers[sr.TaskID]
		streamRegistry.mu.Unlock()
		if exists {
			return
		}

		if _, err := os.Stat(sr.FilePath); err != nil {
			log.Printf("[%s] stream request: file not found: %s", sr.TaskID[:8], sr.FilePath)
			return
		}

		srv := engine.NewStreamServerFromDisk(sr.FilePath, 0)
		streamURL, err := srv.Start(context.Background())
		if err != nil {
			log.Printf("[%s] stream failed: %v", sr.TaskID[:8], err)
			return
		}

		streamRegistry.mu.Lock()
		streamRegistry.servers[sr.TaskID] = srv
		streamRegistry.mu.Unlock()

		log.Printf("[%s] streaming from disk: %s → %s", sr.TaskID[:8], filepath.Base(sr.FilePath), streamURL)

		// Report stream URL back to the server via transport
		go func() {
			if _, err := transport.SendProgress(ctx, agent.StatusUpdate{
				TaskID:    sr.TaskID,
				StreamURL: streamURL,
			}); err != nil {
				log.Printf("[%s] stream URL report failed: %v", sr.TaskID[:8], err)
			}
		}()
	}

	// Wire: WS control actions (pause/cancel/stream pushed from server)
	d.OnControlAction = func(action, taskID string) {
		switch action {
		case "cancel":
			manager.CancelTask(taskID)
			cancelStreamTask(taskID)
		case "pause":
			manager.PauseTask(taskID)
			cancelStreamTask(taskID)
		case "resume":
			log.Printf("[%s] resume requested via WebSocket, triggering poll", taskID[:8])
			d.TriggerPoll()
		case "stream":
			// Use registry mutex to prevent TOCTOU race with HTTP-polled stream requests
			streamRegistry.mu.Lock()
			if _, exists := streamRegistry.servers[taskID]; exists {
				streamRegistry.mu.Unlock()
				return
			}
			task := manager.GetTask(taskID)
			if task == nil || task.GetStreamURL() != "" {
				streamRegistry.mu.Unlock()
				return
			}
			streamRegistry.mu.Unlock()
			srv, err := torrentDl.StartStream(taskID)
			if err != nil {
				log.Printf("[%s] stream failed: %v", taskID[:8], err)
				return
			}
			streamRegistry.mu.Lock()
			streamRegistry.servers[taskID] = srv
			streamRegistry.mu.Unlock()
			task.SetStreamURL(srv.URL())
		}
	}

	// Wire: server-requested upgrade
	d.OnUpgradeRequested = func(targetVersion string) {

		// Wait for active downloads to finish
		if active := manager.ActiveCount(); active > 0 {
			log.Printf("Waiting for %d active download(s) to finish before upgrading...", active)
			manager.Wait()
		}

		upgrader := &upgrade.Upgrader{CurrentVersion: Version}
		result := upgrader.Execute(ctx, targetVersion)

		// Report result to server
		reportCtx, reportCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer reportCancel()
		errMsg := ""
		if result.Error != nil {
			errMsg = result.Error.Error()
		}
		upgradeResult := agent.UpgradeResult{
			AgentID: cfg.Agent.ID,
			Success: result.Success,
			Version: result.NewVersion,
			Error:   errMsg,
		}
		_ = transport.ReportUpgradeResult(reportCtx, upgradeResult)

		if !result.Success {
			log.Printf("Upgrade failed: %v", result.Error)
			d.ClearUpgradeInProgress()
			return
		}

		// Restart: replace current process with the new binary
		log.Printf("Upgrade successful (%s → %s), restarting...", result.OldVersion, result.NewVersion)

		// Deregister first so the server knows we're restarting
		deregCtx, deregCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer deregCancel()
		_ = transport.Deregister(deregCtx, cfg.Agent.ID)

		// Flush progress reporter
		cancel()

		// Re-exec with the same args — the new binary takes over
		binPath, err := os.Executable()
		if err != nil {
			log.Printf("Could not determine executable path: %v", err)
			os.Exit(75) // EX_TEMPFAIL
		}
		// syscall.Exec replaces the current process (Unix)
		execErr := syscall.Exec(binPath, os.Args, os.Environ())
		// If we get here, exec failed (e.g. Windows)
		log.Printf("Exec failed: %v — exiting for service manager restart", execErr)
		os.Exit(75)
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

	// Start daemon (blocks)
	errCh := make(chan error, 1)
	go func() {
		errCh <- d.Run(ctx)
	}()

	// Wait for signal or error
	select {
	case sig := <-sigCh:
		fmt.Printf("\n  Received %s, shutting down...\n", sig)
		cancel()

		// Give active downloads 30s to finish
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		manager.Shutdown(shutdownCtx)

		fmt.Println("  Daemon stopped.")
		return nil

	case err := <-errCh:
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
