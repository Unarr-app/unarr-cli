package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/torrentclaw-cli/internal/agent"
	"github.com/torrentclaw/torrentclaw-cli/internal/engine"
	"github.com/torrentclaw/torrentclaw-cli/internal/parser"
)

func newDownloadCmd() *cobra.Command {
	var method string

	cmd := &cobra.Command{
		Use:   "download <info_hash|magnet>",
		Short: "Download a torrent (one-shot, no daemon needed)",
		Long: `Download a specific torrent by info hash or magnet link.
This is a standalone download — it does not require the daemon to be running.`,
		Example: `  unarr download abc123def456abc123def456abc123def456abc1
  unarr download "magnet:?xt=urn:btih:..." --method torrent`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDownload(args[0], method)
		},
	}

	cmd.Flags().StringVar(&method, "method", "torrent", "download method: torrent (default)")

	return cmd
}

func runDownload(input, method string) error {
	cfg := loadConfig()
	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)

	// Parse input
	parsed := parser.Parse(input)
	infoHash := parsed.InfoHash
	if infoHash == "" {
		// Treat as info hash directly if 40 hex chars
		input = strings.TrimSpace(input)
		if len(input) == 40 {
			infoHash = strings.ToLower(input)
		} else {
			return fmt.Errorf("invalid input: provide a 40-char info hash or magnet URI")
		}
	}

	outputDir := cfg.Download.Dir
	if outputDir == "" {
		home, _ := os.UserHomeDir()
		outputDir = home
	}

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	fmt.Println()
	bold.Printf("  Downloading %s...\n", infoHash[:16]+"...")
	fmt.Printf("  Method: %s | Output: %s\n", method, outputDir)
	fmt.Println()

	// Create torrent downloader
	torrentDl, err := engine.NewTorrentDownloader(engine.TorrentConfig{
		DataDir:      outputDir,
		StallTimeout: 90 * time.Second,
		MaxTimeout:   60 * time.Minute,
		SeedEnabled:  false,
	})
	if err != nil {
		return fmt.Errorf("create downloader: %w", err)
	}

	// Create a dummy reporter (no API reporting for one-shot)
	reporter := engine.NewProgressReporter(
		agent.NewClient(cfg.Auth.APIURL, cfg.Auth.APIKey, "unarr/"+Version),
		5*time.Second,
	)

	manager := engine.NewManager(engine.ManagerConfig{
		MaxConcurrent: 1,
		OutputDir:     outputDir,
		Organize: engine.OrganizeConfig{
			Enabled:    cfg.Organize.Enabled,
			MoviesDir:  cfg.Organize.MoviesDir,
			TVShowsDir: cfg.Organize.TVShowsDir,
		},
	}, reporter, torrentDl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n  Cancelling download...")
		cancel()
	}()

	// Start progress reporter
	go reporter.Run(ctx)

	// Submit task
	task := agent.Task{
		ID:              "oneshot-" + infoHash[:8],
		InfoHash:        infoHash,
		Title:           parsed.Name,
		PreferredMethod: method,
	}

	manager.Submit(ctx, task)
	manager.Wait()

	// Check result
	active := manager.ActiveTasks()
	if len(active) == 0 {
		green.Println("  Download complete!")
	} else {
		for _, t := range active {
			if t.GetStatus() == engine.StatusFailed {
				return fmt.Errorf("download failed: %s", t.ErrorMessage)
			}
		}
	}

	// Shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	manager.Shutdown(shutdownCtx)
	cancel()

	log.SetOutput(os.Stderr) // suppress cleanup logs
	fmt.Println()

	return nil
}
