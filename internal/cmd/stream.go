package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/torrentclaw-cli/internal/engine"
	"github.com/torrentclaw/torrentclaw-cli/internal/parser"
	"github.com/torrentclaw/torrentclaw-cli/internal/ui"
)

func newStreamCmd() *cobra.Command {
	var (
		port      int
		noOpen    bool
		playerCmd string
	)

	cmd := &cobra.Command{
		Use:   "stream <magnet|infohash>",
		Short: "Stream a torrent directly to a media player",
		Long: `Stream a torrent by info hash or magnet link without waiting for the full download.

Downloads pieces sequentially (prioritizing the beginning of the file) and serves
the video over a local HTTP server. Automatically detects and opens mpv, vlc, or
your default browser.

The stream server runs until you press Ctrl+C. Data is stored temporarily in your
download directory (or system temp if not configured).`,
		Example: `  unarr stream abc123def456abc123def456abc123def456abc1
  unarr stream "magnet:?xt=urn:btih:..." --port 8080
  unarr stream <hash> --player mpv
  unarr stream <hash> --no-open`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStream(args[0], port, noOpen, playerCmd)
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "HTTP server port (default: random available)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "don't open a player, just print the URL")
	cmd.Flags().StringVar(&playerCmd, "player", "", "media player command (default: auto-detect)")
	cmd.RegisterFlagCompletionFunc("player", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"mpv\tmpv media player", "vlc\tVLC media player"}, cobra.ShellCompDirectiveNoFileComp
	})

	return cmd
}

func runStream(input string, port int, noOpen bool, playerCmd string) error {
	cfg := loadConfig()
	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	yellow := color.New(color.FgYellow)
	dim := color.New(color.FgHiBlack)

	// Parse input
	parsed := parser.Parse(input)
	magnetOrHash := input
	if parsed.InfoHash != "" && !parsed.IsMagnet {
		magnetOrHash = parsed.InfoHash
	} else if parsed.InfoHash == "" {
		trimmed := strings.TrimSpace(input)
		if len(trimmed) == 40 {
			magnetOrHash = strings.ToLower(trimmed)
		} else if !strings.HasPrefix(trimmed, "magnet:") {
			return fmt.Errorf("invalid input: provide a 40-char info hash or magnet URI")
		}
	}

	// Data directory
	dataDir := cfg.Download.Dir
	if dataDir == "" {
		dataDir = filepath.Join(os.TempDir(), "unarr-stream")
	}

	// Create engine
	eng, err := engine.NewStreamEngine(engine.StreamConfig{
		DataDir:     dataDir,
		Port:        port,
		MetaTimeout: 60 * time.Second,
		NoOpen:      noOpen,
		PlayerCmd:   playerCmd,
	})
	if err != nil {
		return fmt.Errorf("create stream engine: %w", err)
	}

	// Signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n  Shutting down...")
		cancel()
	}()

	// Header
	fmt.Println()
	bold.Println("  unarr Stream")
	fmt.Println()

	// Start engine (metadata + file selection)
	dim.Println("  Waiting for metadata...")
	if err := eng.Start(ctx, magnetOrHash); err != nil {
		eng.Shutdown(context.Background())
		return err
	}

	fileName := eng.FileName()
	fileSize := eng.FileLength()
	bold.Printf("  File: %s (%s)\n", fileName, ui.FormatBytes(fileSize))

	if !eng.IsVideoFile() {
		yellow.Println("  Warning: no video files found, streaming largest file")
	}

	// Start HTTP server
	srv := engine.NewStreamServer(eng, port)
	streamURL, err := srv.Start(ctx)
	if err != nil {
		eng.Shutdown(context.Background())
		return fmt.Errorf("start server: %w", err)
	}

	fmt.Printf("  URL: %s\n", streamURL)
	fmt.Println()

	// Buffer before opening player
	dim.Print("  Buffering...")
	err = eng.WaitBuffer(ctx, func(buffered, target int64) {
		pct := int(float64(buffered) / float64(target) * 100)
		if pct > 100 {
			pct = 100
		}
		fmt.Printf("\r  Buffering: %d%% (%s / %s)  ",
			pct, ui.FormatBytes(buffered), ui.FormatBytes(target))
	})
	if err != nil {
		srv.Shutdown(context.Background())
		eng.Shutdown(context.Background())
		return err
	}
	fmt.Println()

	// Start progress tracking
	eng.StartProgressLoop(ctx)

	// Open player
	if !noOpen {
		playerName, _, openErr := engine.OpenPlayer(streamURL, playerCmd)
		if openErr != nil {
			yellow.Printf("  Could not open player: %s\n", openErr)
			fmt.Printf("  Open this URL in your player: %s\n", streamURL)
		} else {
			green.Printf("  Opened in %s\n", playerName)
		}
	} else {
		fmt.Printf("  Open this URL in your player: %s\n", streamURL)
	}
	fmt.Println()

	// Progress loop until Ctrl+C
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	completed := false
	for {
		select {
		case <-ctx.Done():
			goto shutdown
		case <-ticker.C:
			p := eng.Progress()
			pct := 0
			if p.TotalBytes > 0 {
				pct = int(float64(p.DownloadedBytes) / float64(p.TotalBytes) * 100)
			}
			fmt.Printf("\r  %d%% | %s/s | Peers: %d | Seeds: %d  ",
				pct, ui.FormatBytes(p.SpeedBps), p.Peers, p.Seeds)

			if pct >= 100 && !completed {
				completed = true
				fmt.Println()
				green.Println("  Download complete! Stream server still running. Ctrl+C to exit.")
			}
		}
	}

shutdown:
	fmt.Println()
	fmt.Println()
	dim.Println("  Cleaning up...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	srv.Shutdown(shutdownCtx)
	eng.Shutdown(shutdownCtx)

	fmt.Println("  Done.")
	fmt.Println()
	return nil
}
