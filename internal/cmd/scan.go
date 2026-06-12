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

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/config"
	"github.com/torrentclaw/unarr/internal/library"
	"github.com/torrentclaw/unarr/internal/library/mediainfo"
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
			cfg := loadConfig()
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// All scanned roots feed ONE sync session (single syncStartedAt +
			// final isLastBatch) so the server's stale-row cleanup sees the
			// whole cycle at once. fullCycle only without an explicit path —
			// a subtree scan must never let the server reap outside it.
			if len(args) == 0 {
				paths := library.ResolveScanPaths(cfg.Download.Dir, cfg.Organize.MoviesDir, cfg.Organize.TVShowsDir, cfg.Library.ScanPath)
				if len(paths) == 0 {
					return fmt.Errorf("usage: unarr scan <path>\n\nNo scan paths configured. Provide a path or set up downloads.dir via 'unarr init'")
				}
				var items []agent.LibrarySyncItem
				var caches []*library.LibraryCache
				for _, p := range paths {
					cache, err := runScan(ctx, cfg, p, workers, ffprobe)
					if err != nil {
						return err
					}
					caches = append(caches, cache)
					items = append(items, library.BuildSyncItems(cache)...)
				}
				if noSync || jsonOut {
					return nil
				}
				if err := syncToServer(ctx, cfg, items, paths, true); err != nil {
					return err
				}
				if ac := scanAPIClient(cfg); ac != nil {
					for _, cache := range caches {
						detectAndSubmitSkipSegments(ctx, cfg, ac, cache)
					}
				}
				return nil
			}
			cache, err := runScan(ctx, cfg, args[0], workers, ffprobe)
			if err != nil {
				return err
			}
			if noSync || jsonOut {
				return nil
			}
			if err := syncToServer(ctx, cfg, library.BuildSyncItems(cache), []string{args[0]}, false); err != nil {
				return err
			}
			if ac := scanAPIClient(cfg); ac != nil {
				detectAndSubmitSkipSegments(ctx, cfg, ac, cache)
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&workers, "workers", 0, "concurrent ffprobe workers (default: config or 8)")
	cmd.Flags().StringVar(&ffprobe, "ffprobe", "", "path to ffprobe binary")
	cmd.Flags().BoolVar(&showStatus, "status", false, "show summary of last scan")
	cmd.Flags().BoolVar(&noSync, "no-sync", false, "scan only, don't upload to server")

	return cmd
}

// runScan walks one root, saves the cache and prewarms sidecars. Syncing to
// the server is the CALLER's job (RunE) — all roots of an invocation feed one
// sync session via syncToServer, so per-root sessions can't trick the server
// into reaping rows of roots the session never visited.
func runScan(ctx context.Context, cfg config.Config, dirPath string, workers int, ffprobePath string) (*library.LibraryCache, error) {
	// Validate path
	info, err := os.Stat(dirPath)
	if err != nil {
		return nil, fmt.Errorf("path not found: %s", dirPath)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", dirPath)
	}

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
		return nil, fmt.Errorf("scan failed: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\r\033[K") // clear progress line

	// Save cache
	if err := library.SaveCache(cache); err != nil {
		return nil, fmt.Errorf("save cache: %w", err)
	}

	// Remember scan path in config
	if cfg.Library.ScanPath != dirPath {
		cfg.Library.ScanPath = dirPath
		_ = config.Save(cfg, cfgFile)
	}

	// Print summary
	printScanSummary(cache)

	// JSON output mode — emit the cache and skip the prewarm (the caller skips
	// the sync via the same flag).
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return cache, enc.Encode(cache)
	}

	// Pre-extract sidecars (text subs → WebVTT, panel frames → JPEG) into a hidden
	// ".unarr" dir so playback gets instant subtitles/thumbnails and huge remuxes
	// never hit the on-demand timeout. Best-effort + Ctrl-C interruptible (the scan
	// itself is already saved).
	if cfg.Library.CacheSubtitles || cfg.Library.CacheThumbnails || cfg.Library.Trickplay.Enabled {
		if ff, err := mediainfo.ResolveFFmpeg(cfg.Library.FFmpegPath); err == nil {
			fmt.Fprintf(os.Stderr, "  Pre-extracting subtitles + thumbnails to cache… (Ctrl-C to skip)\n")
			library.PrewarmSidecars(ctx, cache, library.PrewarmOptions{
				FFmpegPath:           ff,
				CacheSubtitles:       cfg.Library.CacheSubtitles,
				CacheThumbnails:      cfg.Library.CacheThumbnails,
				Workers:              2,
				Trickplay:            cfg.Library.Trickplay.Enabled,
				TrickplayIntervalSec: cfg.Library.Trickplay.IntervalSeconds(),
				TrickplayWidth:       cfg.Library.Trickplay.Width,
				MaxLoadRatio:         cfg.Library.PrewarmMaxLoadRatio,
			})
		} else {
			fmt.Fprintf(os.Stderr, "  Skipping sidecar prewarm: ffmpeg unavailable: %v\n", err)
		}
	}

	return cache, nil
}

// scanAPIClient builds the agent API client for post-scan submissions, using
// the same key resolution as syncToServer. Nil when no key is configured.
func scanAPIClient(cfg config.Config) *agent.Client {
	apiKey := apiKeyFlag
	if apiKey == "" {
		apiKey = cfg.Auth.APIKey
	}
	if apiKey == "" {
		return nil
	}
	return agent.NewClient(cfg.Auth.APIURL, apiKey, "unarr/"+Version)
}

// syncToServer uploads the scanned items of THIS invocation as one sync
// session. roots lists every root the invocation scanned; fullCycle marks a
// no-args run that covered all configured roots (the server may then reap
// stale rows regardless of prefix — see LibrarySyncRequest.FullCycle).
func syncToServer(ctx context.Context, cfg config.Config, items []agent.LibrarySyncItem, roots []string, fullCycle bool) error {
	apiKey := apiKeyFlag
	if apiKey == "" {
		apiKey = cfg.Auth.APIKey
	}
	if apiKey == "" {
		color.Yellow("\n  ⚠ No API key configured. Run 'unarr init' to set up, or use --no-sync.")
		return nil
	}

	ac := agent.NewClient(cfg.Auth.APIURL, apiKey, "unarr/"+Version)

	if len(items) == 0 {
		color.Yellow("\n  No valid items to sync.")
		return nil
	}

	res, err := library.SyncBatches(ctx, ac, items, library.SyncOptions{
		AgentID:   cfg.Agent.ID,
		ScanPath:  roots[0],
		ScanRoots: roots,
		FullCycle: fullCycle,
		OnProgress: func(sent, total int) {
			fmt.Fprintf(os.Stderr, "\r  Syncing %d/%d items...\033[K", sent, total)
		},
	})
	if err != nil {
		return fmt.Errorf("sync failed: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\r\033[K")

	green := color.New(color.FgGreen)
	green.Printf("\n  ✓ Synced %d items (%d matched, %d removed)\n", res.Synced, res.Matched, res.Removed)

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

		res := library.ResolveResolution(item.MediaInfo.Video.Width, item.MediaInfo.Video.Height)
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
