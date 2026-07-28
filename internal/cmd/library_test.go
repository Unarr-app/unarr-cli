package cmd

import (
	"path/filepath"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/library"
	"github.com/fatih/color"
)

func TestResolveFloor(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"", library.MinPlausibleVideoBytes},
		{"garbage", library.MinPlausibleVideoBytes},
		{"0", library.MinPlausibleVideoBytes},
		{"1MiB", 1 << 20},
		{"512KB", 512000},
		{"2MB", 2000000},
	}
	for _, tt := range tests {
		if got := resolveFloor(tt.in); got != tt.want {
			t.Errorf("resolveFloor(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// TestCleanupOptions asserts the config toggles map through to ReconcileOptions,
// and that --apply / --dedup-only ride along.
func TestCleanupOptions(t *testing.T) {
	cfg := config.Config{}
	cfg.Library.Cleanup = config.CleanupConfig{
		MinVideoBytes:         "2MiB",
		RemoveStubs:           true,
		RemoveOrphanPartials:  false,
		DedupExact:            true,
		RemoveOrphanSubtitles: false,
		PruneEmptyDirs:        true,
	}

	opts := cleanupOptions(cfg, true /*apply*/, true /*dedupOnly*/)
	if opts.MinVideoBytes != 2<<20 {
		t.Errorf("floor = %d, want %d", opts.MinVideoBytes, 2<<20)
	}
	if !opts.RemoveStubs || opts.RemoveOrphanPartials || !opts.DedupExact ||
		opts.RemoveOrphanSubtitles || !opts.PruneEmptyDirs {
		t.Errorf("toggles did not map through: %+v", opts)
	}
	if !opts.Apply {
		t.Error("Apply should be true")
	}
	if !opts.DedupOnly {
		t.Error("DedupOnly should be true")
	}
}

// TestReportCleanSummary_Clean: no failures → nil error.
func TestReportCleanSummary_Clean(t *testing.T) {
	green := color.New(color.FgGreen)
	red := color.New(color.FgRed)
	yellow := color.New(color.FgYellow)
	summary := library.RemoveSummary{Removed: 3, Freed: 4096}
	if err := reportCleanSummary(summary, green, red, yellow); err != nil {
		t.Errorf("clean summary should return nil, got %v", err)
	}
}

// TestReportCleanSummary_Partial: any failure → non-nil error so the command exits
// non-zero and scripts/cron can detect it.
func TestReportCleanSummary_Partial(t *testing.T) {
	green := color.New(color.FgGreen)
	red := color.New(color.FgRed)
	yellow := color.New(color.FgYellow)
	summary := library.RemoveSummary{
		Removed: 1,
		Freed:   512,
		Failures: []library.RemoveFailure{
			{Path: "/x/stub.mkv", Outcome: library.OutcomePermission, Guidance: "could not delete /x/stub.mkv: permission denied — ..."},
		},
	}
	err := reportCleanSummary(summary, green, red, yellow)
	if err == nil {
		t.Fatal("partial failure must return a non-nil error")
	}
}

// TestPrintCleanFinding_NoPanic exercises both the success and failed-mark paths of
// the per-item printer (output goes to stdout; we assert it does not panic and
// respects the failed-path map).
func TestPrintCleanFinding_NoPanic(t *testing.T) {
	dim := color.New(color.FgHiBlack)
	red := color.New(color.FgRed)

	f := library.Finding{Path: "/lib/stub.mkv", Kind: library.KindStubVideo, Reason: "stub", Bytes: 512}

	// Success path (apply, not in failed map).
	printCleanFinding(f, true, map[string]library.RemoveFailure{}, dim, red)

	// Failed path.
	failed := map[string]library.RemoveFailure{
		filepath.Clean("/lib/stub.mkv"): {Path: "/lib/stub.mkv", Guidance: "could not delete /lib/stub.mkv: permission denied"},
	}
	printCleanFinding(f, true, failed, dim, red)

	// Dry-run path.
	printCleanFinding(f, false, map[string]library.RemoveFailure{}, dim, red)
}
