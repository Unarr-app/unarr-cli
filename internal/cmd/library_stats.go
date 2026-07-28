package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Unarr-app/unarr-cli/internal/library"
	"github.com/Unarr-app/unarr-cli/internal/ui"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

func newLibraryStatsCmd() *cobra.Command {
	var (
		jsonFlag bool
		workers  int
	)

	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Report library health, composition, and quality (read-only)",
		Long: `Report the health and composition of your Movies/TV library and download
directory. This is a pure DRY-RUN: it only READS — nothing on disk is ever
modified.

Three blocks are reported:

  Composition — number of movies, shows, seasons and episodes, plus the REAL
                on-disk space (allocated blocks, like ` + "`du`" + `) per category and
                the average size per title.
  Health      — the same sweep 'unarr library clean' would perform, in dry-run:
                stubs, orphaned partials, duplicates, orphaned sidecars, empty
                dirs and media-named dirs — with the total space reclaimable.
  Quality     — resolution (2160p/1080p/720p/…), video codec (h265/h264/…) and
                HDR breakdown, extracted with ffprobe. A file ffprobe can't read
                is counted as "unknown" and never aborts the report. Probing the
                whole library can take a while on a large collection.

Everything is confined to the configured download/movies/tv directories.`,
		Example: `  unarr library stats               # readable table (dry-run — reads only)
  unarr library stats --json        # emit the full stats struct as JSON
  unarr library stats --workers 4   # limit concurrent ffprobe workers`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLibraryStats(jsonFlag, workers)
		},
	}

	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit the full stats struct as JSON (for scripts)")
	cmd.Flags().IntVar(&workers, "workers", 0, "concurrent ffprobe workers for the quality pass (default: config or 8)")
	return cmd
}

func runLibraryStats(jsonFlag bool, workers int) error {
	cfg := loadConfig()
	paths := library.ReconcilePaths{
		DownloadDir: cfg.Download.Dir,
		MoviesDir:   cfg.Organize.MoviesDir,
		TVShowsDir:  cfg.Organize.TVShowsDir,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if workers == 0 {
		workers = cfg.Library.Workers
	}

	// A large library can take a while to ffprobe end-to-end — tell an interactive
	// user so a slow quality pass doesn't look like a hang. Suppressed for --json
	// (stderr chatter would still be there, but keep the machine path quiet).
	if !jsonFlag {
		color.New(color.FgHiBlack).Fprintln(os.Stderr,
			"  Probing library for quality stats — this can take a while on a large collection…")
	}

	// Report is a full picture of what's reclaimable, so every hygiene category is
	// evaluated regardless of which ones the user toggled OFF for the daemon's
	// auto-sweep under [library.cleanup]. Only the anti-stub floor is honoured from
	// config. ComputeStats forces Apply=false, so this stays a pure dry-run.
	reconcile := library.DefaultReconcileOptions()
	reconcile.MinVideoBytes = resolveFloor(cfg.Library.Cleanup.MinVideoBytes)

	stats, err := library.ComputeStats(ctx, library.StatsOptions{
		Paths:       paths,
		Reconcile:   reconcile,
		Workers:     workers,
		FFprobePath: cfg.Library.FFprobePath,
	})
	if err != nil {
		return err
	}

	if jsonFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(stats)
	}

	printLibraryStats(stats)
	return nil
}

// printLibraryStats renders the three blocks as readable tables in the same style
// as `unarr status` / `library clean` (bold headers, dim rules, fatih/color).
func printLibraryStats(s *library.LibraryStats) {
	bold := color.New(color.Bold)

	fmt.Println()
	bold.Println("  unarr library stats")
	fmt.Println()

	printCompositionBlock(s.Composition, bold)
	printHealthBlock(s.Health, bold)
	printQualityBlock(s.Quality, bold)
}

func printCompositionBlock(c library.CompositionStats, bold *color.Color) {
	dim := color.New(color.FgHiBlack)
	bold.Println("  Composition")
	dim.Println("  ────────────────────────────────────────────")
	fmt.Printf("  %-22s%d\n", "Movies", c.Movies)
	fmt.Printf("  %-22s%d\n", "Shows", c.Shows)
	fmt.Printf("  %-22s%d\n", "Seasons", c.Seasons)
	fmt.Printf("  %-22s%d\n", "Episodes", c.Episodes)
	fmt.Println()
	fmt.Printf("  %-22s%s\n", "Movies size", ui.FormatBytes(c.MovieBytes))
	fmt.Printf("  %-22s%s\n", "TV Shows size", ui.FormatBytes(c.TVBytes))
	if c.DownloadBytes > 0 {
		// The download-dir remainder OUTSIDE Movies/TV (raw releases not yet
		// organized). When the download dir is an ancestor of Movies/TV, those
		// nested files are already counted under Movies/TV — this is only the rest.
		fmt.Printf("  %-22s%s\n", "Downloads (unsorted)", ui.FormatBytes(c.DownloadBytes))
	}
	bold.Printf("  %-22s%s\n", "Total on disk", ui.FormatBytes(c.TotalBytes))
	fmt.Println()
	if c.Movies > 0 {
		dim.Printf("  %-22s%s\n", "Avg / movie", ui.FormatBytes(c.AvgMovieBytes))
	}
	if c.Episodes > 0 {
		dim.Printf("  %-22s%s\n", "Avg / episode", ui.FormatBytes(c.AvgEpisodeBytes))
	}
	fmt.Println()
}

func printHealthBlock(h library.HealthStats, bold *color.Color) {
	dim := color.New(color.FgHiBlack)
	green := color.New(color.FgGreen)
	yellow := color.New(color.FgYellow)

	bold.Println("  Health / reclaimable")
	dim.Println("  ────────────────────────────────────────────")
	if h.TotalFindings == 0 {
		green.Println("  Nothing to clean — download & library dirs are tidy.")
		fmt.Println()
		return
	}
	for _, cat := range h.Categories {
		fmt.Printf("  %-22s%3d   %s\n", healthKindLabel(cat.Kind), cat.Count, ui.FormatBytes(cat.Bytes))
	}
	fmt.Println()
	bold.Printf("  %-22s%3d   %s\n", "Reclaimable", h.TotalFindings, ui.FormatBytes(h.ReclaimableBytes))
	yellow.Println("  run: unarr library clean --apply")
	// Suspect (zero-content) videos are reported for AWARENESS: they are a
	// heuristic, not auto-removed by the daemon, and only deleted by
	// `library clean --apply` with remove_corrupt_videos enabled. Call that out so
	// the user knows they're in the count above but need opt-in to reclaim.
	if suspect := healthCategoryCount(h, string(library.KindCorruptVideo)); suspect > 0 {
		dim.Printf("  (%d suspect zero-content video(s) — enable remove_corrupt_videos to reclaim)\n", suspect)
	}
	fmt.Println()
}

// healthKindLabel maps a raw reconcile kind to a human-readable Health-block label.
// An unmapped kind falls back to its raw string so a future kind still renders.
func healthKindLabel(kind string) string {
	switch library.FindingKind(kind) {
	case library.KindStubVideo:
		return "Stubs"
	case library.KindOrphanPartial:
		return "Orphan partials"
	case library.KindOrphanSidecar:
		return "Orphan sidecars"
	case library.KindEmptyDir:
		return "Empty dirs"
	case library.KindMediaNamedDir:
		return "Media-named dirs"
	case library.KindDuplicate:
		return "Duplicates"
	case library.KindCorruptVideo:
		return "Suspect (zero-content)"
	default:
		return kind
	}
}

// healthCategoryCount returns the finding count for a given kind (0 if absent).
func healthCategoryCount(h library.HealthStats, kind string) int {
	for _, c := range h.Categories {
		if c.Kind == kind {
			return c.Count
		}
	}
	return 0
}

func printQualityBlock(q library.QualityStats, bold *color.Color) {
	dim := color.New(color.FgHiBlack)

	bold.Println("  Quality")
	dim.Println("  ────────────────────────────────────────────")
	if q.Total == 0 {
		dim.Println("  No video files found.")
		fmt.Println()
		return
	}

	dim.Println("  Resolution")
	for _, res := range []string{"2160p", "1080p", "720p", "480p", "SD", "unknown"} {
		if n := q.ByResolution[res]; n > 0 {
			fmt.Printf("    %-20s%d\n", res, n)
		}
	}
	fmt.Println()

	dim.Println("  Codec")
	for _, codec := range []string{"h265", "h264", "av1", "other", "unknown"} {
		if n := q.ByCodec[codec]; n > 0 {
			fmt.Printf("    %-20s%d\n", codec, n)
		}
	}
	fmt.Println()

	dim.Println("  Dynamic range")
	fmt.Printf("    %-20s%d\n", "HDR", q.HDR)
	fmt.Printf("    %-20s%d\n", "SDR", q.SDR)
	if q.Unknown > 0 {
		fmt.Printf("    %-20s%d\n", "unknown", q.Unknown)
	}
	fmt.Println()
	dim.Printf("  %d video(s) analyzed.\n\n", q.Total)
}
