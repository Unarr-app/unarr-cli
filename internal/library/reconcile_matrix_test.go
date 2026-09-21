package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file is the COMPLETE table-driven matrix for reconcile: every FindingKind,
// every config toggle on/off, dry-run vs --apply vs --dedup-only, the edge cases
// (floor boundary, .unarr sidecars, 3+ identical + 1 distinct, unicode/space/paren
// names, active vs old partial, out-of-root rejection), and idempotency. It is
// portable — no chmod/symlink/permission assumptions live here; those POSIX-only
// checks are in reconcile_posix_test.go behind a build tag.
//
// Helpers writeSized / writeVideoWithMarker are shared from reconcile_test.go.

const mib = 1024 * 1024

// mustExist / mustGone assert final FS state after a reconcile.
func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected %s to still exist: %v", filepath.Base(path), err)
	}
}

func mustGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed (err=%v)", filepath.Base(path), err)
	}
}

// allOff returns an options set with every category disabled (the zero value's
// intent) but a valid floor, so a test can flip exactly one toggle on.
func allOff() ReconcileOptions {
	return ReconcileOptions{MinVideoBytes: MinPlausibleVideoBytes}
}

// --- Per-FindingKind emission -------------------------------------------------

// TestReconcileEachKind seeds ONE anomaly per case and asserts the finding kind
// (dry-run: reported, not removed).
func TestReconcileEachKind(t *testing.T) {
	tests := []struct {
		name  string
		seed  func(t *testing.T, root string) string // returns the anomalous path
		want  FindingKind
		isDir bool
		// opts overrides the default all-on options for the case. Nil → default.
		// Used by orphan_sidecar to keep PruneEmptyDirs OFF: a loose sidecar always
		// lives in a video-less dir, which (with pruning on) becomes an empty_dir
		// finding that absorbs the sidecar via dropFindingsUnderDirs — so to test
		// orphan_sidecar in isolation we disable the empty-dir pass for that case.
		opts *ReconcileOptions
	}{
		{
			name: "stub_video",
			seed: func(t *testing.T, root string) string {
				p := filepath.Join(root, "stub.mkv")
				writeSized(t, p, 512)
				return p
			},
			want: KindStubVideo,
		},
		{
			name: "orphan_partial",
			seed: func(t *testing.T, root string) string {
				p := filepath.Join(root, "downloading.part")
				writeSized(t, p, 2048)
				return p
			},
			want: KindOrphanPartial,
		},
		{
			// The debrid provenance sidecar ends in ".part.meta.json", so matching
			// on the extension alone ('.json') left orphaned sidecars with NO reaper
			// — they accreted in the download dir forever.
			name: "orphan_part_meta_sidecar",
			seed: func(t *testing.T, root string) string {
				p := filepath.Join(root, "movie.mkv.part.meta.json")
				writeSized(t, p, 180)
				return p
			},
			want: KindOrphanPartial,
		},
		{
			// A loose sidecar with no owning video in its dir → orphaned. Ownership
			// is dir-level (any real video in the dir owns its sidecars), so the dir
			// MUST be video-less for the sidecar to be an orphan — which makes it an
			// empty_dir too. We disable PruneEmptyDirs for this case so the sidecar
			// surfaces as a standalone orphan_sidecar instead of being absorbed.
			name: "orphan_sidecar",
			seed: func(t *testing.T, root string) string {
				p := filepath.Join(root, "loose", "orphan.srt")
				writeSized(t, p, 100)
				return p
			},
			want: KindOrphanSidecar,
			opts: func() *ReconcileOptions { o := DefaultReconcileOptions(); o.PruneEmptyDirs = false; return &o }(),
		},
		{
			name: "empty_dir",
			seed: func(t *testing.T, root string) string {
				p := filepath.Join(root, "emptyshow")
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				return p
			},
			want:  KindEmptyDir,
			isDir: true,
		},
		{
			name: "media_named_dir",
			seed: func(t *testing.T, root string) string {
				p := filepath.Join(root, "movie.mkv")
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				return p
			},
			want:  KindMediaNamedDir,
			isDir: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := tt.seed(t, root)

			opts := DefaultReconcileOptions()
			if tt.opts != nil {
				opts = *tt.opts
			}
			findings, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts)
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			var got *Finding
			for i := range findings {
				if filepath.Clean(findings[i].Path) == filepath.Clean(path) {
					got = &findings[i]
					break
				}
			}
			if got == nil {
				t.Fatalf("kind %s: no finding for %s (findings: %+v)", tt.want, path, findings)
			}
			if got.Kind != tt.want {
				t.Errorf("kind = %s, want %s", got.Kind, tt.want)
			}
			if got.IsDir != tt.isDir {
				t.Errorf("IsDir = %v, want %v", got.IsDir, tt.isDir)
			}
			// Dry-run: never removed.
			mustExist(t, path)
		})
	}
}

// TestReconcileDuplicateKind covers the duplicate_video kind on its own (it needs
// two identical files, so it doesn't fit the one-seed table above).
func TestReconcileDuplicateKind(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "ep.mkv")
	b := filepath.Join(root, "ep (2).mkv")
	writeVideoWithMarker(t, a, 3*mib, 'X')
	writeVideoWithMarker(t, b, 3*mib, 'X')

	findings, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, DefaultReconcileOptions())
	if err != nil {
		t.Fatal(err)
	}
	var dup int
	for _, f := range findings {
		if f.Kind == KindDuplicate {
			dup++
		}
	}
	if dup != 1 {
		t.Errorf("expected 1 duplicate finding, got %d", dup)
	}
}

// --- Per-toggle on/off --------------------------------------------------------

// TestReconcileToggles asserts each category toggle gates its own kind: ON →
// flagged, OFF → left alone. Table drives one toggle at a time.
func TestReconcileToggles(t *testing.T) {
	tests := []struct {
		name   string
		seed   func(t *testing.T, root string) string
		enable func(o *ReconcileOptions)
		kind   FindingKind
	}{
		{
			name: "RemoveStubs",
			seed: func(t *testing.T, root string) string {
				p := filepath.Join(root, "s.mkv")
				writeSized(t, p, 512)
				return p
			},
			enable: func(o *ReconcileOptions) { o.RemoveStubs = true },
			kind:   KindStubVideo,
		},
		{
			name: "RemoveOrphanPartials",
			seed: func(t *testing.T, root string) string {
				p := filepath.Join(root, "x.part")
				writeSized(t, p, 2048)
				return p
			},
			enable: func(o *ReconcileOptions) { o.RemoveOrphanPartials = true },
			kind:   KindOrphanPartial,
		},
		{
			name: "RemoveOrphanSubtitles",
			seed: func(t *testing.T, root string) string {
				p := filepath.Join(root, "l", "o.srt")
				writeSized(t, p, 50)
				return p
			},
			enable: func(o *ReconcileOptions) { o.RemoveOrphanSubtitles = true },
			kind:   KindOrphanSidecar,
		},
		{
			name: "PruneEmptyDirs",
			seed: func(t *testing.T, root string) string {
				p := filepath.Join(root, "empty")
				_ = os.MkdirAll(p, 0o755)
				return p
			},
			enable: func(o *ReconcileOptions) { o.PruneEmptyDirs = true },
			kind:   KindEmptyDir,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/on", func(t *testing.T) {
			root := t.TempDir()
			p := tt.seed(t, root)
			o := allOff()
			tt.enable(&o)
			findings, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, o)
			if err != nil {
				t.Fatal(err)
			}
			if !hasKind(findings, tt.kind) {
				t.Errorf("%s ON: expected a %s finding, got %+v", tt.name, tt.kind, findings)
			}
			_ = p
		})
		t.Run(tt.name+"/off", func(t *testing.T) {
			root := t.TempDir()
			tt.seed(t, root)
			o := allOff() // toggle left off
			findings, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, o)
			if err != nil {
				t.Fatal(err)
			}
			if hasKind(findings, tt.kind) {
				t.Errorf("%s OFF: expected NO %s finding, got %+v", tt.name, tt.kind, findings)
			}
		})
	}
}

// TestReconcileDedupExactToggle: DedupExact off → identical copies are kept.
func TestReconcileDedupExactToggle(t *testing.T) {
	for _, on := range []bool{true, false} {
		name := "off"
		if on {
			name = "on"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			a := filepath.Join(root, "ep.mkv")
			b := filepath.Join(root, "ep (2).mkv")
			writeVideoWithMarker(t, a, 3*mib, 'D')
			writeVideoWithMarker(t, b, 3*mib, 'D')

			o := allOff()
			o.DedupExact = on
			findings, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, o)
			if err != nil {
				t.Fatal(err)
			}
			got := hasKind(findings, KindDuplicate)
			if got != on {
				t.Errorf("DedupExact=%v: hasDuplicate=%v, want %v", on, got, on)
			}
		})
	}
}

func hasKind(findings []Finding, k FindingKind) bool {
	for _, f := range findings {
		if f.Kind == k {
			return true
		}
	}
	return false
}

// --- Dry-run vs apply vs dedup-only ------------------------------------------

// TestReconcileModes is the dry-run/apply/dedup-only matrix over a fixture that
// carries a stub + a duplicate + a healthy video.
func TestReconcileModes(t *testing.T) {
	setup := func(t *testing.T) (root, stub, dupA, dupKeep, healthy string) {
		root = t.TempDir()
		stub = filepath.Join(root, "stub.mkv")
		writeSized(t, stub, 512)
		dupKeep = filepath.Join(root, "movie", "Film.mkv")
		dupA = filepath.Join(root, "movie", "Film (2).mkv")
		writeVideoWithMarker(t, dupKeep, 2*mib, 'M')
		writeVideoWithMarker(t, dupA, 2*mib, 'M')
		healthy = filepath.Join(root, "keep", "Keep.mkv")
		writeSized(t, healthy, 2*mib)
		return
	}

	t.Run("dry-run touches nothing", func(t *testing.T) {
		root, stub, dupA, dupKeep, healthy := setup(t)
		opts := DefaultReconcileOptions() // Apply=false
		if _, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts); err != nil {
			t.Fatal(err)
		}
		mustExist(t, stub)
		mustExist(t, dupA)
		mustExist(t, dupKeep)
		mustExist(t, healthy)
	})

	t.Run("apply removes stub+dup, keeps canonical+healthy", func(t *testing.T) {
		root, stub, dupA, dupKeep, healthy := setup(t)
		opts := DefaultReconcileOptions()
		opts.Apply = true
		if _, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts); err != nil {
			t.Fatal(err)
		}
		mustGone(t, stub)
		mustGone(t, dupA)
		mustExist(t, dupKeep)
		mustExist(t, healthy)
	})

	t.Run("dedup-only removes only the dup", func(t *testing.T) {
		root, stub, dupA, dupKeep, healthy := setup(t)
		opts := DefaultReconcileOptions()
		opts.Apply = true
		opts.DedupOnly = true
		if _, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts); err != nil {
			t.Fatal(err)
		}
		mustExist(t, stub) // stub untouched — not the dedup pass
		mustGone(t, dupA)
		mustExist(t, dupKeep)
		mustExist(t, healthy)
	})
}

// --- Edge cases ---------------------------------------------------------------

// TestReconcileFloorBoundary: a file EXACTLY at the floor is a real video, one
// byte under is a stub.
func TestReconcileFloorBoundary(t *testing.T) {
	root := t.TempDir()
	atFloor := filepath.Join(root, "at.mkv")
	underFloor := filepath.Join(root, "under.mkv")
	writeSized(t, atFloor, int(MinPlausibleVideoBytes)) // == floor → kept
	writeSized(t, underFloor, int(MinPlausibleVideoBytes)-1)

	opts := DefaultReconcileOptions()
	opts.Apply = true
	if _, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts); err != nil {
		t.Fatal(err)
	}
	mustExist(t, atFloor)
	mustGone(t, underFloor)
}

// TestReconcileSampleUnderFloorWithRealVideo: a sample stub next to a real video —
// the stub goes, the real video AND its dir stay.
func TestReconcileSampleUnderFloorWithRealVideo(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Release")
	sample := filepath.Join(dir, "sample.mkv")
	real := filepath.Join(dir, "Feature.mkv")
	writeSized(t, sample, 4096) // stub
	writeSized(t, real, 2*mib)

	opts := DefaultReconcileOptions()
	opts.Apply = true
	if _, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts); err != nil {
		t.Fatal(err)
	}
	mustGone(t, sample)
	mustExist(t, real)
	mustExist(t, dir) // dir has a real video → not pruned
}

// TestReconcileThreeIdenticalOneDistinct: 3 byte-identical + 1 distinct (same size,
// different content) → 2 removed, canonical + distinct kept.
func TestReconcileThreeIdenticalOneDistinct(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "TV", "Show", "S01")
	const size = 3 * mib
	keep := filepath.Join(dir, "Show - S01E01.mkv")
	d1 := filepath.Join(dir, "Show - S01E01 (2).mkv")
	d2 := filepath.Join(dir, "Show - S01E01 [torrent].mkv")
	distinct := filepath.Join(dir, "Show - S01E01 [2160p].mkv")
	writeVideoWithMarker(t, keep, size, 'A')
	writeVideoWithMarker(t, d1, size, 'A')
	writeVideoWithMarker(t, d2, size, 'A')
	writeVideoWithMarker(t, distinct, size, 'B') // same size, different fingerprint

	opts := DefaultReconcileOptions()
	opts.Apply = true
	if _, err := Reconcile(ReconcilePaths{TVShowsDir: filepath.Join(root, "TV")}, nil, opts); err != nil {
		t.Fatal(err)
	}
	mustExist(t, keep)
	mustGone(t, d1)
	mustGone(t, d2)
	mustExist(t, distinct) // different content → NEVER removed
}

// TestReconcileUnarrSidecarParent covers both .unarr branches in a table.
func TestReconcileUnarrSidecarParent(t *testing.T) {
	tests := []struct {
		name       string
		parentHasV bool
		wantKept   bool
	}{
		{"parent has video → sidecar kept", true, true},
		{"parent video-less → sidecar orphaned", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			rel := filepath.Join(root, "Release")
			if tt.parentHasV {
				writeSized(t, filepath.Join(rel, "Release.mkv"), 2*mib)
			} else {
				writeSized(t, filepath.Join(rel, "release.nfo"), 50)
			}
			sub := filepath.Join(rel, ".unarr", "track0.vtt")
			writeSized(t, sub, 200)

			opts := DefaultReconcileOptions()
			opts.Apply = true
			if _, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts); err != nil {
				t.Fatal(err)
			}
			if tt.wantKept {
				mustExist(t, sub)
			} else {
				mustGone(t, sub)
			}
		})
	}
}

// TestReconcileSidecarWithSiblingVideo: a .srt next to its video (same dir) is NOT
// an orphan.
func TestReconcileSidecarWithSiblingVideo(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Movie")
	video := filepath.Join(dir, "Movie.mkv")
	sub := filepath.Join(dir, "Movie.srt")
	writeSized(t, video, 2*mib)
	writeSized(t, sub, 100)

	opts := DefaultReconcileOptions()
	opts.Apply = true
	if _, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts); err != nil {
		t.Fatal(err)
	}
	mustExist(t, sub)
	mustExist(t, video)
}

// TestReconcileOutOfRootRejected: a finding whose path resolves outside the roots
// is refused by applyFindings even if somehow flagged. We simulate by pointing the
// reconcile root at an EMPTY subdir while a sibling file lives outside it.
func TestReconcileOutOfRootConfinement(t *testing.T) {
	base := t.TempDir()
	scanRoot := filepath.Join(base, "scan")
	if err := os.MkdirAll(scanRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// A stub OUTSIDE the scan root — reconcile must never see or remove it.
	outside := filepath.Join(base, "outside.mkv")
	writeSized(t, outside, 512)
	// A stub INSIDE — this one is fair game.
	inside := filepath.Join(scanRoot, "inside.mkv")
	writeSized(t, inside, 512)

	opts := DefaultReconcileOptions()
	opts.Apply = true
	findings, err := Reconcile(ReconcilePaths{DownloadDir: scanRoot}, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if strings.Contains(f.Path, "outside.mkv") {
			t.Errorf("reconcile leaked outside the root: %s", f.Path)
		}
	}
	mustExist(t, outside) // untouched
	mustGone(t, inside)
}

// TestReconcileExoticNames: names with spaces, unicode, and parentheses are handled
// like any other (flagged as stub / deduped) without path breakage.
func TestReconcileExoticNames(t *testing.T) {
	names := []string{
		"El Niño (2014).mkv",
		"space name .mkv",
		"日本語のタイトル.mkv",
		"movie [1080p] (dir's cut).mkv",
	}
	root := t.TempDir()
	for _, n := range names {
		writeSized(t, filepath.Join(root, n), 512) // all stubs
	}
	opts := DefaultReconcileOptions()
	opts.Apply = true
	if _, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		mustGone(t, filepath.Join(root, n))
	}
}

// TestReconcileActiveVsOldPartial: an active partial is protected, an unlisted one
// is reaped. (Idempotent with TestReconcileActivePartialProtected but drives both
// arms in one table.)
func TestReconcileActiveVsOldPartial(t *testing.T) {
	root := t.TempDir()
	activeP := filepath.Join(root, "live.part")
	oldP := filepath.Join(root, "dead.part")
	writeSized(t, activeP, 4096)
	writeSized(t, oldP, 4096)

	active := map[string]bool{filepath.Clean(activeP): true}
	opts := DefaultReconcileOptions()
	opts.Apply = true
	if _, err := Reconcile(ReconcilePaths{DownloadDir: root}, active, opts); err != nil {
		t.Fatal(err)
	}
	mustExist(t, activeP)
	mustGone(t, oldP)
}

// --- Idempotency --------------------------------------------------------------

// TestReconcileIdempotent: run apply twice; the second run finds nothing.
func TestReconcileIdempotent(t *testing.T) {
	root := t.TempDir()
	writeSized(t, filepath.Join(root, "stub.mkv"), 512)
	writeSized(t, filepath.Join(root, "x.part"), 2048)
	writeSized(t, filepath.Join(root, "loose", "o.srt"), 50)
	dupKeep := filepath.Join(root, "d", "Film.mkv")
	dupA := filepath.Join(root, "d", "Film (2).mkv")
	writeVideoWithMarker(t, dupKeep, 2*mib, 'Z')
	writeVideoWithMarker(t, dupA, 2*mib, 'Z')
	_ = os.MkdirAll(filepath.Join(root, "empty"), 0o755)

	opts := DefaultReconcileOptions()
	opts.Apply = true

	first, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("first pass should have found anomalies")
	}

	second, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Errorf("second pass must be a no-op, found %d: %+v", len(second), second)
	}
}

// --- dropFindingsUnderDirs (double-count guard) -------------------------------

// TestDropFindingsUnderDirs is the unit test for the child-absorption fix: a file
// finding inside a dir finding is dropped (its bytes were already tallied in the
// dir), while a file finding OUTSIDE every dir finding is kept.
func TestDropFindingsUnderDirs(t *testing.T) {
	dir := filepath.Join("root", "empty")
	inside := filepath.Join(dir, "downloading.part") // covered by the dir finding
	outside := filepath.Join("root", "loose.srt")    // outside every dir finding

	findings := []Finding{
		{Path: dir, Kind: KindEmptyDir, Bytes: 25_000_000_000, IsDir: true},
		{Path: inside, Kind: KindOrphanPartial, Bytes: 25_000_000_000, IsDir: false},
		{Path: outside, Kind: KindOrphanSidecar, Bytes: 100, IsDir: false},
	}

	got := dropFindingsUnderDirs(findings)

	// Exactly the dir finding + the outside file survive; the inside file is gone.
	if len(got) != 2 {
		t.Fatalf("expected 2 findings after drop, got %d: %+v", len(got), got)
	}
	var haveDir, haveOutside, haveInside bool
	var total int64
	for _, f := range got {
		total += f.Bytes
		switch filepath.Clean(f.Path) {
		case filepath.Clean(dir):
			haveDir = true
		case filepath.Clean(outside):
			haveOutside = true
		case filepath.Clean(inside):
			haveInside = true
		}
	}
	if !haveDir || !haveOutside {
		t.Errorf("dir and outside findings must be kept (dir=%v outside=%v)", haveDir, haveOutside)
	}
	if haveInside {
		t.Error("the finding inside the dir must be dropped (double-count)")
	}
	// Bytes are NOT doubled: dir (25 GB) + outside (100), not + the inside 25 GB.
	if want := int64(25_000_000_000 + 100); total != want {
		t.Errorf("total bytes = %d, want %d (the inside file must not be counted twice)", total, want)
	}
}

// TestDropFindingsUnderDirs_NoDirs: with no dir findings, the slice is returned
// unchanged (fast path).
func TestDropFindingsUnderDirs_NoDirs(t *testing.T) {
	findings := []Finding{
		{Path: "a.mkv", Kind: KindStubVideo, Bytes: 512},
		{Path: "b.part", Kind: KindOrphanPartial, Bytes: 2048},
	}
	got := dropFindingsUnderDirs(findings)
	if len(got) != 2 {
		t.Errorf("no dir findings → unchanged, got %d", len(got))
	}
}

// TestDropFindingsUnderDirs_SiblingPrefixNotAbsorbed: a file in a SIBLING dir that
// shares a name prefix with a flagged dir must NOT be absorbed (path-boundary
// safety — "empty2/x" is not inside "empty").
func TestDropFindingsUnderDirs_SiblingPrefixNotAbsorbed(t *testing.T) {
	flaggedDir := filepath.Join("root", "empty")
	siblingFile := filepath.Join("root", "empty2", "x.part") // shares "empty" prefix but different dir
	findings := []Finding{
		{Path: flaggedDir, Kind: KindEmptyDir, Bytes: 1000, IsDir: true},
		{Path: siblingFile, Kind: KindOrphanPartial, Bytes: 2048, IsDir: false},
	}
	got := dropFindingsUnderDirs(findings)
	if len(got) != 2 {
		t.Errorf("sibling-prefix file must survive, got %d: %+v", len(got), got)
	}
}

// TestReconcileOrphanPartialDirTwoSweeps: a video-less dir holding an ORPHAN .part
// is not removed wholesale as empty_dir — the partial is judged by its own
// category (which knows the active set and the paused-download guard). First sweep
// reaps the .part (counted once); the dir it leaves empty goes on the next sweep.
func TestReconcileOrphanPartialDirTwoSweeps(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "DeadRelease")
	part := filepath.Join(dir, "downloading.part")
	writeSized(t, part, 3*mib) // a chunky partial, no active task, in a video-less dir

	findings, summary, err := ReconcileWithSummary(ReconcilePaths{DownloadDir: root}, nil, applyOpts())
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Kind != KindOrphanPartial {
		t.Fatalf("expected exactly the orphan partial, got %+v", findings)
	}
	if summary.Freed >= 2*3*mib {
		t.Errorf("freed=%d looks double-counted (want ~%d)", summary.Freed, 3*mib)
	}
	mustGone(t, part)
	mustExist(t, dir)

	if _, _, err := ReconcileWithSummary(ReconcilePaths{DownloadDir: root}, nil, applyOpts()); err != nil {
		t.Fatal(err)
	}
	mustGone(t, dir)
}

// TestReconcileNeverPrunesActiveDownloadDir is the regression for the auto-sweep
// deleting a live download: a debrid download writes <release>/<file>.mkv.part, so
// the release dir holds no video until it finishes. With the .part in the active
// set the sweep must leave the dir — and the .part — exactly where they are.
// (Found in a 177-episode debrid run: 18 downloads failed as "storage unavailable:
// could not read back the finished download" after their dir was RemoveAll'd.)
func TestReconcileNeverPrunesActiveDownloadDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "The.Walking.Dead.6x04.HD720p-lat")
	part := filepath.Join(dir, "The.Walking.Dead.6x04.HD720p-lat.mkv.part")
	writeSized(t, part, 3*mib)
	active := map[string]bool{filepath.Clean(part): true}

	findings, err := Reconcile(ReconcilePaths{DownloadDir: root}, active, applyOpts())
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("a live download's dir must not be touched, got %+v", findings)
	}
	mustExist(t, part)
	mustExist(t, dir)
}

// TestReconcileKeepsSubdirsOfActiveDownload: a video-less subdir next to an
// in-progress .part (a release's Extras/ or Sample/). Keeping only the release dir
// is not enough — the empty_dir pass must not descend and prune its children out
// from under the download. (Today only debrid writes .part, one file per release
// dir, so this guards the shape rather than a layout seen in the field.)
func TestReconcileKeepsSubdirsOfActiveDownload(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Show.S01.1080p")
	part := filepath.Join(dir, "Show.S01E01.mkv.part")
	subs := filepath.Join(dir, "Extras", "readme.txt")
	writeSized(t, part, 3*mib)
	writeSized(t, subs, 2048)
	active := map[string]bool{filepath.Clean(part): true}

	findings, err := Reconcile(ReconcilePaths{DownloadDir: root}, active, applyOpts())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.Kind == KindEmptyDir {
			t.Errorf("pruned %s inside a live download", f.Path)
		}
	}
	mustExist(t, subs)
}

// TestReconcileKeepsPausedDownloadDir: a PAUSED download's .part has a frozen mtime
// (so it is not in the recent-activity active set) and the daemon switches the
// orphan-partial category off while any download is pending. The empty_dir pass
// used to ignore both and RemoveAll the dir — destroying the resume data.
func TestReconcileKeepsPausedDownloadDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Paused.Release")
	part := filepath.Join(dir, "Paused.Release.mkv.part")
	writeSized(t, part, 3*mib)
	opts := applyOpts()
	opts.RemoveOrphanPartials = false // what orphanPartialRemovalAllowed does while a download is pending

	findings, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("a paused download's dir must survive, got %+v", findings)
	}
	mustExist(t, part)
}

func applyOpts() ReconcileOptions {
	o := DefaultReconcileOptions()
	o.Apply = true
	return o
}

// --- Configured-root protection (organize targets must survive) ---------------

// TestReconcileProtectsConfiguredRoots is the regression for the e2e-caught bug:
// download dir is the PARENT of the Movies/TV dirs (download=/media,
// movies=/media/Movies), and on an EMPTY library the sweep must NOT flag or delete
// Movies/ or TV Shows/ — they are configured organize targets that have to survive
// even when momentarily empty.
func TestReconcileProtectsConfiguredRoots(t *testing.T) {
	root := t.TempDir()
	movies := filepath.Join(root, "Movies")
	tv := filepath.Join(root, "TV Shows")
	if err := os.MkdirAll(movies, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tv, 0o755); err != nil {
		t.Fatal(err)
	}

	paths := ReconcilePaths{DownloadDir: root, MoviesDir: movies, TVShowsDir: tv}
	opts := DefaultReconcileOptions()
	opts.Apply = true

	findings, err := Reconcile(paths, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	// No finding may name a configured dir.
	for _, f := range findings {
		clean := filepath.Clean(f.Path)
		if clean == filepath.Clean(movies) || clean == filepath.Clean(tv) || clean == filepath.Clean(root) {
			t.Errorf("configured dir must never be flagged, got finding for %s (%s)", f.Path, f.Kind)
		}
	}
	// And they still exist after --apply.
	mustExist(t, movies)
	mustExist(t, tv)
	mustExist(t, root)
}

// TestReconcileConfiguredRootKeepsButCleansJunkChildren: Movies/TV hold only junk
// (a loose stub / partial, no video). The configured dir itself is NOT flagged
// empty_dir, but the junk file INSIDE it IS cleaned. The container survives; its
// stray children go.
func TestReconcileConfiguredRootKeepsButCleansJunkChildren(t *testing.T) {
	root := t.TempDir()
	movies := filepath.Join(root, "Movies")
	tv := filepath.Join(root, "TV Shows")
	if err := os.MkdirAll(movies, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tv, 0o755); err != nil {
		t.Fatal(err)
	}
	// Junk directly inside the configured dirs (no video anywhere).
	moviesStub := filepath.Join(movies, "junk.mkv")
	writeSized(t, moviesStub, 512) // stub
	tvPartial := filepath.Join(tv, "leftover.part")
	writeSized(t, tvPartial, 2048) // orphan partial

	paths := ReconcilePaths{DownloadDir: root, MoviesDir: movies, TVShowsDir: tv}
	opts := DefaultReconcileOptions()
	opts.Apply = true

	findings, err := Reconcile(paths, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		clean := filepath.Clean(f.Path)
		if (clean == filepath.Clean(movies) || clean == filepath.Clean(tv)) && f.Kind == KindEmptyDir {
			t.Errorf("configured dir %s must not be flagged empty_dir", f.Path)
		}
	}
	// Junk children removed; configured containers kept.
	mustGone(t, moviesStub)
	mustGone(t, tvPartial)
	mustExist(t, movies)
	mustExist(t, tv)
}

// TestReconcileNonConfiguredEmptySubdirStillPruned: a NON-configured empty subdir
// inside Movies/ (e.g. Movies/Blah/ with no video) is still flagged empty_dir — the
// protection is only for the configured roots themselves, normal pruning is intact.
func TestReconcileNonConfiguredEmptySubdirStillPruned(t *testing.T) {
	root := t.TempDir()
	movies := filepath.Join(root, "Movies")
	blah := filepath.Join(movies, "Blah")
	if err := os.MkdirAll(blah, 0o755); err != nil {
		t.Fatal(err)
	}

	paths := ReconcilePaths{DownloadDir: root, MoviesDir: movies}
	opts := DefaultReconcileOptions()
	opts.Apply = true

	findings, err := Reconcile(paths, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFindingFor(findings, blah) {
		t.Errorf("a non-configured empty subdir %s should still be flagged empty_dir; findings=%+v", blah, findings)
	}
	mustGone(t, blah)    // pruned
	mustExist(t, movies) // configured dir kept
}

// TestApplyFindingsRefusesConfiguredRoot: even if a configured root somehow reaches
// applyFindings as a finding (defense in depth), it is NOT removed.
func TestApplyFindingsRefusesConfiguredRoot(t *testing.T) {
	root := t.TempDir()
	movies := filepath.Join(root, "Movies")
	if err := os.MkdirAll(movies, 0o755); err != nil {
		t.Fatal(err)
	}
	roots := []string{filepath.Clean(root), filepath.Clean(movies)}

	// Hand-craft a finding that targets the configured Movies dir.
	findings := []Finding{
		{Path: movies, Kind: KindEmptyDir, Bytes: 0, IsDir: true},
	}
	summary := applyFindings(findings, roots)

	mustExist(t, movies) // guard must have refused it
	if summary.Removed != 0 {
		t.Errorf("Removed=%d, want 0 (a configured root must never be removed)", summary.Removed)
	}
}

// TestIsConfiguredRoot table-drives the exact-match helper: true for each root
// (including with a trailing separator / un-cleaned form), false for a subdir or
// sibling.
func TestIsConfiguredRoot(t *testing.T) {
	sep := string(os.PathSeparator)
	base := filepath.Join("srv", "media")
	movies := filepath.Join(base, "Movies")
	tv := filepath.Join(base, "TV Shows")
	roots := []string{base, movies, tv}

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"download root exact", base, true},
		{"movies root exact", movies, true},
		{"tv root exact", tv, true},
		{"movies with trailing sep", movies + sep, true},                    // filepath.Clean strips it
		{"movies un-cleaned dot segment", filepath.Join(movies, "."), true}, // Clean → movies
		{"subdir of movies", filepath.Join(movies, "Blah"), false},
		{"sibling of movies", filepath.Join(base, "Other"), false},
		{"prefix-sharing sibling", movies + "2", false}, // "Movies2" is not "Movies"
		{"unrelated path", filepath.Join("tmp", "x"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isConfiguredRoot(filepath.Clean(tt.in), roots); got != tt.want {
				t.Errorf("isConfiguredRoot(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// hasFindingFor reports whether any finding targets the given path.
func hasFindingFor(findings []Finding, path string) bool {
	for _, f := range findings {
		if filepath.Clean(f.Path) == filepath.Clean(path) {
			return true
		}
	}
	return false
}

// --- Video-extension recognition (data-loss guard) ----------------------------

// TestReconcileRecognizesAllVideoContainers is the reconcile-side regression for
// the divergent-extension bug: a dir holding ONLY a real video of each container
// type (>= floor) must NOT be flagged empty_dir and must survive --apply. Before
// the unification, a .m2ts (Blu-ray remux) was unrecognised → its dir judged
// video-less → RemoveAll deleted the film.
func TestReconcileRecognizesAllVideoContainers(t *testing.T) {
	exts := []string{".mkv", ".mp4", ".avi", ".wmv", ".mov", ".flv", ".webm",
		".m4v", ".ts", ".m2ts", ".mpg", ".mpeg", ".vob"}
	for _, ext := range exts {
		t.Run(ext, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "Movie")
			video := filepath.Join(dir, "Feature"+ext)
			writeSized(t, video, 2*mib) // a real video

			opts := DefaultReconcileOptions()
			opts.Apply = true
			findings, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range findings {
				if f.Kind == KindEmptyDir && filepath.Clean(f.Path) == filepath.Clean(dir) {
					t.Errorf("%s: dir with a real video was flagged empty_dir", ext)
				}
			}
			mustExist(t, video) // the video must survive
			mustExist(t, dir)
		})
	}
}

// TestIsVideoExtSuperset asserts the canonical set recognises .mpg/.mpeg/.vob (were
// missing from engine's list) AND .m2ts (was missing from library's list).
func TestIsVideoExtSuperset(t *testing.T) {
	for _, ext := range []string{".m2ts", ".mpg", ".mpeg", ".vob"} {
		if !IsVideoExt("x" + ext) {
			t.Errorf("IsVideoExt must recognise %s", ext)
		}
	}
	if IsVideoExt("x.srt") || IsVideoExt("x.nfo") {
		t.Error("IsVideoExt must not recognise sidecar extensions")
	}
}

// TestReconcileNoRootsError: no configured roots is an explicit error, not a panic.
func TestReconcileNoRootsError(t *testing.T) {
	_, err := Reconcile(ReconcilePaths{}, nil, DefaultReconcileOptions())
	if err == nil {
		t.Error("expected an error when no roots are configured")
	}
}

// TestReconcileWithSummaryFreedBytes: on a clean apply, summary.Freed equals the
// sum of removed findings' bytes and there are no failures.
func TestReconcileWithSummaryFreedBytes(t *testing.T) {
	root := t.TempDir()
	writeSized(t, filepath.Join(root, "a.mkv"), 512)
	writeSized(t, filepath.Join(root, "b.part"), 4096)

	opts := DefaultReconcileOptions()
	opts.Apply = true
	findings, summary, err := ReconcileWithSummary(ReconcilePaths{DownloadDir: root}, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Failures) != 0 {
		t.Errorf("clean apply should have no failures, got %d", len(summary.Failures))
	}
	if summary.Removed != len(findings) {
		t.Errorf("Removed=%d, want %d", summary.Removed, len(findings))
	}
	var want int64
	for _, f := range findings {
		want += f.Bytes
	}
	if summary.Freed != want {
		t.Errorf("Freed=%d, want %d", summary.Freed, want)
	}
}

// TestReconcileSafeAppliesOnlySafe: ReconcileSafe applies safe kinds and returns a
// summary; here everything is safe, so it all goes.
func TestReconcileSafeAppliesOnlySafe(t *testing.T) {
	root := t.TempDir()
	stub := filepath.Join(root, "s.mkv")
	writeSized(t, stub, 512)

	safe, freed, summary, err := ReconcileSafe(ReconcilePaths{DownloadDir: root}, nil, DefaultReconcileOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(safe) == 0 {
		t.Fatal("expected the stub to be reconciled as safe")
	}
	if freed != summary.Freed {
		t.Errorf("freed=%d != summary.Freed=%d", freed, summary.Freed)
	}
	mustGone(t, stub)
}

// TestRecentPartialProtectionByMtime documents the mtime-window contract the daemon
// relies on (a partial modified moments ago is active). Reconcile itself takes the
// active set as input; this asserts a freshly-written partial's mtime is within a
// short window so recentPartials (daemon) would flag it.
func TestFreshPartialIsRecent(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "x.part")
	writeSized(t, p, 1024)
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(info.ModTime()) > time.Minute {
		t.Errorf("a just-written partial should look recent, mtime age = %v", time.Since(info.ModTime()))
	}
}
