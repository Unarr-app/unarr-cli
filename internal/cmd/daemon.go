package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/torrentclaw-cli/internal/agent"
	"github.com/torrentclaw/torrentclaw-cli/internal/config"
	"github.com/torrentclaw/torrentclaw-cli/internal/engine"
)

// newStartCmd creates the top-level `unarr start` command.
func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the download daemon (foreground)",
		Long: `Start the unarr daemon in the foreground.

Registers with the server, polls for download tasks, and executes them
using the configured download method. Press Ctrl+C to stop gracefully.`,
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
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("  Use Ctrl+C in the terminal where the daemon is running.")
			fmt.Println("  (Signal-based stop coming in a future release)")
			return nil
		},
	}
}

// newDaemonCmd creates `unarr daemon` for administrative subcommands.
func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Daemon administration (install, uninstall, logs)",
		Long:  "Administrative commands for managing the daemon as a system service.",
	}

	cmd.AddCommand(
		newDaemonInstallCmd(),
		newDaemonUninstallCmd(),
	)

	return cmd
}

func newDaemonInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install daemon as a system service (systemd/launchd)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("  Service installation coming in a future release.")
			fmt.Println("  For now, use: unarr start")
			return nil
		},
	}
}

func newDaemonUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove daemon system service",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("  Service uninstall coming in a future release.")
			return nil
		},
	}
}

func runDaemonStart() error {
	cfg := loadConfig()
	bold := color.New(color.Bold)

	// Validate config
	if cfg.Auth.APIKey == "" {
		return fmt.Errorf("no API key configured — run 'unarr setup' first")
	}
	if cfg.Agent.ID == "" {
		return fmt.Errorf("no agent ID — run 'unarr setup' first")
	}
	if cfg.Download.Dir == "" {
		return fmt.Errorf("no download directory — run 'unarr setup' first")
	}

	// Validate configured paths are safe
	if err := cfg.ValidatePaths(); err != nil {
		return fmt.Errorf("unsafe configuration: %w", err)
	}

	// Ensure download dir exists
	if err := os.MkdirAll(cfg.Download.Dir, 0o755); err != nil {
		return fmt.Errorf("create download dir: %w", err)
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

	// Create agent client
	ac := agent.NewClient(cfg.Auth.APIURL, cfg.Auth.APIKey, "unarr/"+Version)

	// Create daemon
	daemonCfg := agent.DaemonConfig{
		AgentID:           cfg.Agent.ID,
		AgentName:         cfg.Agent.Name,
		Version:           Version,
		DownloadDir:       cfg.Download.Dir,
		PollInterval:      pollInterval,
		HeartbeatInterval: heartbeatInterval,
	}
	d := agent.NewDaemon(daemonCfg, ac)

	// Create progress reporter
	reporter := engine.NewProgressReporter(ac, 3*time.Second)

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
	}, reporter, torrentDl)

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
