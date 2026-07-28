package cmd

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/library"
	"github.com/Unarr-app/unarr-cli/internal/ui"
	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// newLibraryCmd is the parent for library-maintenance subcommands. Kept as a group
// (`unarr library <sub>`) so future maintenance verbs (scan, stats, …) slot in
// without crowding the top-level command list.
func newLibraryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "library",
		GroupID: "system",
		Short:   "Library maintenance (clean up orphaned files, …)",
		Long:    `Maintenance commands for your Movies/TV library and download directory.`,
	}
	cmd.AddCommand(newLibraryCleanCmd())
	cmd.AddCommand(newLibraryStatsCmd())
	return cmd
}

func newLibraryCleanCmd() *cobra.Command {
	var (
		apply     bool
		dryRun    bool
		dedupOnly bool
	)

	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Sweep the download & library dirs for orphaned files (dry-run by default)",
		Long: `Scan the download directory and the Movies/TV library dirs for hygiene
anomalies and report them. Idempotent and DRY-RUN by default — nothing is
touched until you pass --apply.

Reports (and with --apply, removes):
  - Video files below the plausibility floor (default 1 MiB) — download stubs
  - Orphaned partials (.part, .!qB, .aria2, .tmp, .partial) with no active task
  - Byte-identical duplicate videos in a dir (kept: one canonical copy)
  - Subtitles/sidecars (.srt, .nfo, .jpg, .par2, …) with no owning video
  - Directories that contain no valid video (empty or only junk)
  - Directories whose NAME is a media filename (e.g. "movie.mkv/")

A valid video file (>= the floor) is NEVER removed, and duplicates are only
removed after a full byte-for-byte compare confirms they are identical (a cheap
fingerprint just finds the candidates). Everything acted upon is confined to the
configured download/movies/tv directories.

The same sweep runs AUTOMATICALLY after each library auto-scan in the daemon
(configurable under [library.cleanup] in config.toml). This command is the
manual entry point and also lets you preview (dry-run) before applying.

Categories are configured under [library.cleanup]; disabled categories are
skipped here too.`,
		Example: `  unarr library clean               # report only (dry-run)
  unarr library clean --apply       # actually remove the orphans
  unarr library clean --dedup-only  # only collapse byte-identical duplicates
  unarr library clean --dedup-only --apply`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLibraryClean(apply && !dryRun, dedupOnly)
		},
	}

	cmd.Flags().BoolVar(&apply, "apply", false, "remove the reported files (default: report only)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "force report-only even with --apply (explicit preview)")
	cmd.Flags().BoolVar(&dedupOnly, "dedup-only", false, "only collapse byte-identical duplicate videos")
	return cmd
}

// cleanupOptions builds reconcile options from the [library.cleanup] config,
// resolving the human-readable floor. It respects each category toggle so the
// manual command mirrors what the daemon would auto-apply.
func cleanupOptions(cfg config.Config, apply, dedupOnly bool) library.ReconcileOptions {
	c := cfg.Library.Cleanup
	floor := resolveFloor(c.MinVideoBytes)

	opts := library.OptionsFrom(floor,
		c.RemoveStubs, c.RemoveOrphanPartials, c.DedupExact, c.RemoveOrphanSubtitles, c.PruneEmptyDirs)
	opts.Apply = apply
	opts.DedupOnly = dedupOnly
	return opts
}

// resolveFloor parses the human-readable min-video size ("1MiB", "512KB"), falling
// back to the library floor (1 MiB) on empty/unparseable input.
func resolveFloor(s string) int64 {
	if s == "" {
		return library.MinPlausibleVideoBytes
	}
	n, err := humanize.ParseBytes(s)
	if err != nil || n == 0 {
		return library.MinPlausibleVideoBytes
	}
	return int64(n)
}

func runLibraryClean(apply, dedupOnly bool) error {
	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	yellow := color.New(color.FgYellow)
	red := color.New(color.FgRed)
	dim := color.New(color.FgHiBlack)

	// A running daemon may hold in-flight partials. Only --apply is blocked (it could
	// delete a file out from under an active download); a dry-run is always safe and
	// still protects live partials by mtime, exactly like the daemon's own auto-sweep,
	// so the user can preview even while the daemon runs.
	daemonAlive := false
	if state := agent.ReadState(); state != nil && state.PID > 0 && agent.IsProcessAlive(state.PID) {
		daemonAlive = true
		if apply {
			return fmt.Errorf("daemon is running (PID %d) — stop it first with 'unarr stop' before --apply so an in-flight download isn't deleted (a dry-run is fine while it runs)", state.PID)
		}
	}

	cfg := loadConfig()
	paths := library.ReconcilePaths{
		DownloadDir: cfg.Download.Dir,
		MoviesDir:   cfg.Organize.MoviesDir,
		TVShowsDir:  cfg.Organize.TVShowsDir,
	}

	// When previewing with the daemon up, protect partials touched in the last few
	// minutes (a live download writes continuously) so they aren't reported as orphans.
	var active map[string]bool
	if daemonAlive {
		active = recentPartials(paths.DownloadDir, 5*time.Minute)
	}

	opts := cleanupOptions(cfg, apply, dedupOnly)
	findings, summary, err := library.ReconcileWithSummary(paths, active, opts)
	if err != nil {
		return err
	}

	fmt.Println()
	bold.Println("  unarr library clean")
	fmt.Println()

	if len(findings) == 0 {
		dim.Println("  Nothing to clean — download & library dirs are tidy.")
		fmt.Println()
		return nil
	}

	// failedPaths lets the per-item listing mark what could NOT be removed (with
	// its guidance) instead of a misleading "removed".
	failedPaths := make(map[string]library.RemoveFailure, len(summary.Failures))
	for _, f := range summary.Failures {
		failedPaths[f.Path] = f
	}

	var totalBytes int64
	for _, f := range findings {
		totalBytes += f.Bytes
		printCleanFinding(f, apply, failedPaths, dim, red)
	}
	fmt.Println()
	bold.Printf("  Total: %d item(s), %s\n", len(findings), ui.FormatBytes(totalBytes))
	fmt.Println()

	if !apply {
		yellow.Println("  Dry run — pass --apply to remove these.")
		fmt.Println()
		return nil
	}
	return reportCleanSummary(summary, green, red, yellow)
}

// printCleanFinding prints one finding line, flagging it as failed (with the
// actionable guidance) when --apply could not remove it. filepath.Clean matches
// the key applyFindings used when recording the failure.
func printCleanFinding(f library.Finding, apply bool, failed map[string]library.RemoveFailure, dim, red *color.Color) {
	mark := "would remove"
	if apply {
		mark = "removed"
	}
	fail, didFail := failed[filepath.Clean(f.Path)]
	if apply && didFail {
		mark = "FAILED"
	}
	fmt.Printf("    %s ", shortenHome(f.Path))
	// f.Bytes is REAL on-disk usage. When a file is sparse (apparent much bigger
	// than what removing it frees), show both so "removed a 1.1 GiB file, freed
	// 4 KiB" is not a mystery.
	if f.Apparent > f.Bytes*2 && f.Apparent-f.Bytes > 1<<20 {
		dim.Printf("(%s — %s, %s on disk / %s apparent, sparse)\n",
			f.Kind, mark, ui.FormatBytes(f.Bytes), ui.FormatBytes(f.Apparent))
	} else {
		dim.Printf("(%s — %s, %s)\n", f.Kind, mark, ui.FormatBytes(f.Bytes))
	}
	if apply && didFail {
		red.Printf("        %s\n", fail.Guidance)
		return
	}
	dim.Printf("        %s\n", f.Reason)
}

// reportCleanSummary prints the apply-phase footer: how many were removed, bytes
// freed, and — for anything that failed — the count plus a reminder that the
// per-item guidance was printed above. A partial failure returns a non-nil error
// so `unarr library clean --apply` exits non-zero (scripts/cron can detect it).
func reportCleanSummary(summary library.RemoveSummary, green, red, yellow *color.Color) error {
	green.Printf("  ✓ Cleaned %d item(s), %s freed\n", summary.Removed, ui.FormatBytes(summary.Freed))
	if len(summary.Failures) == 0 {
		fmt.Println()
		return nil
	}
	red.Printf("  ✗ %d item(s) could not be removed — see the guidance above each one.\n", len(summary.Failures))
	yellow.Println("  Fix the permissions / mount and re-run 'unarr library clean --apply'.")
	fmt.Println()
	return fmt.Errorf("%d of %d item(s) could not be removed", len(summary.Failures), summary.Removed+len(summary.Failures))
}
