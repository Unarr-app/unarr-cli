package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/config"
	"github.com/torrentclaw/unarr/internal/library"
)

func newScanCmd() *cobra.Command {
	var (
		workers    int
		ffprobe    string
		showStatus bool
		noSync     bool
	)

	cmd := &cobra.Command{
		Use:   "scan <path>",
		Short: "Scan your media library for quality analysis",
		Long: `Walk a folder recursively, analyze each video file with ffprobe,
and sync the results to your TorrentClaw account.

After scanning, visit your Library page at torrentclaw.com/library
to see available quality upgrades.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if showStatus {
				return runScanStatus()
			}
			if len(args) == 0 {
				cfg := loadConfig()
				if cfg.Library.ScanPath != "" {
					args = append(args, cfg.Library.ScanPath)
				} else {
					return fmt.Errorf("usage: unarr scan <path>\n\nProvide a media folder to scan")
				}
			}
			return runScan(args[0], workers, ffprobe, noSync)
		},
	}

	cmd.Flags().IntVar(&workers, "workers", 0, "concurrent ffprobe workers (default: config or 8)")
	cmd.Flags().StringVar(&ffprobe, "ffprobe", "", "path to ffprobe binary")
	cmd.Flags().BoolVar(&showStatus, "status", false, "show summary of last scan")
	cmd.Flags().BoolVar(&noSync, "no-sync", false, "scan only, don't upload to server")

	return cmd
}

func runScan(dirPath string, workers int, ffprobePath string, noSync bool) error {
	// Validate path
	info, err := os.Stat(dirPath)
	if err != nil {
		return fmt.Errorf("path not found: %s", dirPath)
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", dirPath)
	}

	cfg := loadConfig()

	// Resolve workers: flag → config → default 8
	if workers == 0 {
		workers = cfg.Library.Workers
	}
	if workers == 0 {
		workers = 8
	}

	// Resolve ffprobe path from flag → config
	if ffprobePath == "" {
		ffprobePath = cfg.Library.FFprobePath
	}

	// Load existing cache for incremental scanning
	existing, _ := library.LoadCache()

	// Context with signal handling
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bold := color.New(color.Bold)
	bold.Printf("\n  Scanning %s...\n\n", dirPath)

	// Scan
	cache, err := library.Scan(ctx, dirPath, existing, library.ScanOptions{
		Workers:     workers,
		FFprobePath: ffprobePath,
		Incremental: existing != nil,
		OnProgress: func(scanned, total int, current string) {
			// Truncate filename for display
			if len(current) > 50 {
				current = "..." + current[len(current)-47:]
			}
			fmt.Fprintf(os.Stderr, "\r  Scanning %d/%d — %s\033[K", scanned, total, current)
		},
	})
	if err != nil {
		return fmt.Errorf("scan failed: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\r\033[K") // clear progress line

	// Save cache
	if err := library.SaveCache(cache); err != nil {
		return fmt.Errorf("save cache: %w", err)
	}

	// Remember scan path in config
	if cfg.Library.ScanPath != dirPath {
		cfg.Library.ScanPath = dirPath
		_ = config.Save(cfg, cfgFile)
	}

	// Print summary
	printScanSummary(cache)

	// JSON output mode
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cache)
	}

	// Sync to server
	if !noSync {
		return syncToServer(ctx, cfg, cache)
	}

	return nil
}

func syncToServer(ctx context.Context, cfg config.Config, cache *library.LibraryCache) error {
	apiKey := apiKeyFlag
	if apiKey == "" {
		apiKey = cfg.Auth.APIKey
	}
	if apiKey == "" {
		color.Yellow("\n  ⚠ No API key configured. Run 'unarr init' to set up, or use --no-sync.")
		return nil
	}

	ac := agent.NewClient(cfg.Auth.APIURL, apiKey, "unarr/"+Version)

	items := library.BuildSyncItems(cache)

	if len(items) == 0 {
		color.Yellow("\n  No valid items to sync.")
		return nil
	}

	// Send in batches of 100
	const batchSize = 100
	totalSynced := 0
	totalMatched := 0
	totalRemoved := 0
	syncStartedAt := time.Now().UTC().Format(time.RFC3339)

	for i := 0; i < len(items); i += batchSize {
		end := i + batchSize
		if end > len(items) {
			end = len(items)
		}
		batch := items[i:end]
		isLast := end >= len(items)

		fmt.Fprintf(os.Stderr, "\r  Syncing %d/%d items...\033[K", end, len(items))

		resp, err := ac.SyncLibrary(ctx, agent.LibrarySyncRequest{
			Items:         batch,
			ScanPath:      cache.Path,
			IsLastBatch:   isLast,
			SyncStartedAt: syncStartedAt,
		})
		if err != nil {
			return fmt.Errorf("sync failed: %w", err)
		}

		totalSynced += resp.Synced
		totalMatched += resp.Matched
		totalRemoved += resp.Removed
	}

	fmt.Fprintf(os.Stderr, "\r\033[K")

	green := color.New(color.FgGreen)
	green.Printf("\n  ✓ Synced %d items (%d matched, %d removed)\n", totalSynced, totalMatched, totalRemoved)

	apiURL := strings.TrimSuffix(cfg.Auth.APIURL, "/")
	fmt.Printf("  → View upgrades at %s/library\n\n", apiURL)

	return nil
}

func runScanStatus() error {
	cache, err := library.LoadCache()
	if err != nil {
		return fmt.Errorf("load cache: %w", err)
	}
	if cache == nil {
		return fmt.Errorf("no library scan found. Run 'unarr scan <path>' first")
	}

	printScanSummary(cache)
	return nil
}

func printScanSummary(cache *library.LibraryCache) {
	bold := color.New(color.Bold)
	dim := color.New(color.Faint)

	total := len(cache.Items)
	errors := 0
	resCount := map[string]int{}
	hdrCount := map[string]int{}
	langCount := map[string]int{}

	for _, item := range cache.Items {
		if item.ScanError != "" {
			errors++
			continue
		}
		if item.MediaInfo == nil || item.MediaInfo.Video == nil {
			continue
		}

		res := library.ResolveResolution(item.MediaInfo.Video.Height)
		if res == "" {
			res = "other"
		}
		resCount[res]++

		hdr := item.MediaInfo.Video.HDR
		if hdr == "" {
			hdr = "SDR"
		}
		hdrCount[hdr]++

		for _, lang := range item.MediaInfo.Languages {
			langCount[lang]++
		}
	}

	bold.Printf("\n  Library scan complete — %d files in %s\n", total, cache.Path)
	dim.Printf("  Scanned at: %s\n\n", cache.ScannedAt)

	// Resolution table
	bold.Println("  Resolution    Files")
	dim.Println("  ─────────────────────")
	for _, res := range []string{"2160p", "1080p", "720p", "480p", "other"} {
		if count, ok := resCount[res]; ok {
			fmt.Printf("  %-14s%d\n", res, count)
		}
	}

	// HDR table
	fmt.Println()
	bold.Println("  HDR           Files")
	dim.Println("  ─────────────────────")
	hdrOrder := []string{"DV+HDR10", "DV", "HDR10", "HLG", "SDR"}
	for _, hdr := range hdrOrder {
		if count, ok := hdrCount[hdr]; ok {
			fmt.Printf("  %-14s%d\n", hdr, count)
		}
	}

	// Top languages
	if len(langCount) > 0 {
		fmt.Println()
		type langEntry struct {
			lang  string
			count int
		}
		var langs []langEntry
		for l, c := range langCount {
			langs = append(langs, langEntry{l, c})
		}
		sort.Slice(langs, func(i, j int) bool { return langs[i].count > langs[j].count })
		top := langs
		if len(top) > 5 {
			top = top[:5]
		}
		parts := make([]string, len(top))
		for i, l := range top {
			parts[i] = fmt.Sprintf("%s (%d)", strings.ToUpper(l.lang), l.count)
		}
		bold.Print("  Top languages: ")
		fmt.Println(strings.Join(parts, ", "))
	}

	if errors > 0 {
		fmt.Println()
		color.Yellow("  Scan errors: %d files (run with --verbose for details)", errors)
	}
	fmt.Println()
}
