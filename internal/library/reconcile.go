package library

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// MinPlausibleVideoBytes is the anti-stub floor for reconcile: a video file below
// this size is a CDN/download stub, not real media, and is a candidate for removal.
// It intentionally mirrors engine.minPlausibleVideoBytes (1 MiB) — the two live in
// separate packages, so the value is duplicated rather than imported. Keep them in
// lockstep: engine's verify()/debrid gate reject stubs at download time, reconcile
// sweeps the ones already on disk from before the gate existed.
const MinPlausibleVideoBytes int64 = 1 << 20 // 1 MiB

// partialExts are in-progress download markers left by torrent/debrid/aria2/usenet
// clients. Without an active task holding them they are dead weight.
var partialExts = map[string]bool{
	".part":    true,
	".!qb":     true, // qBittorrent
	".aria2":   true, // aria2 control file
	".tmp":     true,
	".partial": true,
}

// IsPartialExt reports whether name has an in-progress download extension
// (.part/.!qb/.aria2/.tmp/.partial). Exported so the daemon can identify live
// partials (by mtime) to protect them from the auto-cleanup sweep.
func IsPartialExt(name string) bool {
	return partialExts[strings.ToLower(filepath.Ext(name))]
}

// sidecarReconcileExts are non-video companion files that only make sense next to a
// video of the same basename. Orphaned (no sibling video) they are stragglers.
var sidecarReconcileExts = map[string]bool{
	".srt": true, ".sub": true, ".ass": true, ".ssa": true, ".vtt": true,
	".idx": true, ".nfo": true, ".jpg": true, ".jpeg": true, ".png": true,
	".par2": true,
}

// FindingKind classifies what reconcile found.
type FindingKind string

const (
	KindStubVideo     FindingKind = "stub_video"      // video file below the plausibility floor
	KindOrphanPartial FindingKind = "orphan_partial"  // .part/.!qb/… with no active task
	KindOrphanSidecar FindingKind = "orphan_sidecar"  // subtitle/nfo/art with no sibling video
	KindEmptyDir      FindingKind = "empty_dir"       // dir with no video (empty or only junk)
	KindMediaNamedDir FindingKind = "media_named_dir" // a directory literally named "movie.mkv"
	KindDuplicate     FindingKind = "duplicate_video" // a byte-identical copy of a kept video
)

// safeKinds are the deterministic, unambiguous categories the daemon reconciles
// AUTOMATICALLY (apply) after each scan. Every kind reconcile emits is currently
// safe: a stub is never real media, an orphan partial/sidecar has no owner, a
// video-less dir holds nothing to lose, a media-named dir is a mis-created folder,
// and a duplicate is removed only after a full byte-for-byte compare confirms it
// (the fingerprint is just the cheap filter), keeping one copy. Kept as an explicit
// allow-list so a future risky kind is opt-in.
var safeKinds = map[FindingKind]bool{
	KindStubVideo:     true,
	KindOrphanPartial: true,
	KindOrphanSidecar: true,
	KindEmptyDir:      true,
	KindMediaNamedDir: true,
	KindDuplicate:     true,
}

// IsSafe reports whether a finding is safe to remove automatically (daemon auto-run).
func (f Finding) IsSafe() bool { return safeKinds[f.Kind] }

// Finding is one reconciled anomaly.
type Finding struct {
	Path   string
	Kind   FindingKind
	Reason string
	// Bytes is the REAL on-disk usage (allocated blocks, POSIX), NOT the apparent
	// size — so the reported/"freed" total reflects what actually frees when the
	// path is removed. A sparse .part (1 GiB apparent, ~0 blocks) contributes ~0
	// here. See diskUsage (reconcile_usage_{unix,windows}.go).
	Bytes int64
	// Apparent is the logical file size (info.Size()); kept for the human-readable
	// detail line where it differs meaningfully from Bytes (sparse files). For dirs
	// it is the sum of the children's apparent sizes.
	Apparent int64
	IsDir    bool
}

// ReconcilePaths are the roots reconcile scans and confines itself to. Any empty
// entry is ignored. Everything acted upon must resolve inside one of these.
type ReconcilePaths struct {
	DownloadDir string
	MoviesDir   string
	TVShowsDir  string
}

// roots returns the non-empty, cleaned scan roots.
func (p ReconcilePaths) roots() []string {
	var out []string
	for _, d := range []string{p.DownloadDir, p.MoviesDir, p.TVShowsDir} {
		if d != "" {
			out = append(out, filepath.Clean(d))
		}
	}
	return out
}

// ReconcileOptions configures which hygiene categories run and the anti-stub floor.
// The zero value enables NOTHING; use DefaultReconcileOptions() for the all-on set,
// or build it from config. MinVideoBytes <= 0 falls back to MinPlausibleVideoBytes.
type ReconcileOptions struct {
	MinVideoBytes         int64 // anti-stub floor; <= 0 → MinPlausibleVideoBytes
	RemoveStubs           bool
	RemoveOrphanPartials  bool
	DedupExact            bool
	RemoveOrphanSubtitles bool
	PruneEmptyDirs        bool // also covers media-named dirs
	DedupOnly             bool // when true, ONLY the dedup pass runs (ignores the flags above except DedupExact)
	Apply                 bool // execute removals (false = report only / dry-run)
}

// OptionsFrom builds a ReconcileOptions from the per-category toggles + a resolved
// byte floor. Kept here (not in cmd) so both the manual command and the daemon
// auto-run derive options the same way from config. floor <= 0 → MinPlausibleVideoBytes.
func OptionsFrom(floor int64, removeStubs, removeOrphanPartials, dedupExact, removeOrphanSubtitles, pruneEmptyDirs bool) ReconcileOptions {
	return ReconcileOptions{
		MinVideoBytes:         floor,
		RemoveStubs:           removeStubs,
		RemoveOrphanPartials:  removeOrphanPartials,
		DedupExact:            dedupExact,
		RemoveOrphanSubtitles: removeOrphanSubtitles,
		PruneEmptyDirs:        pruneEmptyDirs,
	}
}

// DefaultReconcileOptions returns the all-categories-on option set (report mode).
func DefaultReconcileOptions() ReconcileOptions {
	return ReconcileOptions{
		MinVideoBytes:         MinPlausibleVideoBytes,
		RemoveStubs:           true,
		RemoveOrphanPartials:  true,
		DedupExact:            true,
		RemoveOrphanSubtitles: true,
		PruneEmptyDirs:        true,
	}
}

// floor returns the effective anti-stub floor.
func (o ReconcileOptions) floor() int64 {
	if o.MinVideoBytes <= 0 {
		return MinPlausibleVideoBytes
	}
	return o.MinVideoBytes
}

// Reconcile scans the configured roots and reports (dry-run) or, when opts.Apply is
// true, removes the enabled hygiene anomalies: download stubs, orphaned partials,
// orphaned sidecars, byte-identical duplicate videos, video-less dirs, and
// directories whose name is itself a media file.
//
// It is idempotent and NEVER touches a valid video (>= the floor). activePartials
// is a set of absolute paths currently held by a running download (so an in-progress
// .part is not reaped); pass an empty map when the daemon is stopped. Every action
// is logged. Confined to roots — nothing outside is touched.
//
// When opts.Apply is set, per-file removal failures (permission denied, read-only
// mount, disconnected NAS) are collected — never fatal — and surfaced via
// ReconcileWithSummary; this thin wrapper drops the summary for callers that only
// want the findings + the walk error.
func Reconcile(paths ReconcilePaths, activePartials map[string]bool, opts ReconcileOptions) ([]Finding, error) {
	findings, _, err := ReconcileWithSummary(paths, activePartials, opts)
	return findings, err
}

// ReconcileWithSummary is Reconcile plus the apply-phase RemoveSummary (removed
// count, bytes freed, and each failure with actionable guidance). The summary is
// zero-valued in dry-run (opts.Apply == false). The walk error is returned only for
// a fatal traversal fault (a failed WalkDir on a root) — individual removal failures
// live in the summary and never abort the sweep.
func ReconcileWithSummary(paths ReconcilePaths, activePartials map[string]bool, opts ReconcileOptions) ([]Finding, RemoveSummary, error) {
	roots := paths.roots()
	if len(roots) == 0 {
		return nil, RemoveSummary{}, fmt.Errorf("no roots configured (download/movies/tv dirs all empty) — nothing to reconcile")
	}
	if activePartials == nil {
		activePartials = map[string]bool{}
	}

	// dedup-only short-circuits the per-file/dir passes.
	if opts.DedupOnly {
		var findings []Finding
		if opts.DedupExact {
			dupFindings, _ := findDuplicateVideos(roots, opts.floor())
			findings = append(findings, dupFindings...)
		}
		return applyAndReturn(findings, roots, opts)
	}

	findings, seenDirs, err := scanRoots(roots, activePartials, opts)
	if err != nil {
		return findings, RemoveSummary{}, err
	}

	// Second pass: byte-identical duplicate videos within the same dir (RC-8). This
	// is the biggest space win — versionDistinctPath historically CLONED a redundant
	// re-download into "S01E03 (2).mkv"/"…[torrent].mkv" siblings instead of
	// deduplicating, leaving ~23 identical copies of an episode in the field. We keep
	// ONE canonical copy and flag the rest. Done as its own pass so it can group by
	// directory across the whole tree.
	if opts.DedupExact {
		dupFindings, _ := findDuplicateVideos(roots, opts.floor())
		findings = append(findings, dupFindings...)
	}

	// Third pass: dirs with no video (empty or only junk). Done after the file walk
	// so we can judge each dir by its current contents. Skips dirs already flagged as
	// media-named and never flags a root itself.
	if opts.PruneEmptyDirs {
		findings = append(findings, findVideolessDirs(roots, seenDirs, opts.floor())...)
	}

	// A dir finding (empty_dir/media_named_dir) is removed with RemoveAll, so any
	// per-file finding INSIDE it (an orphan .part, a stub, a sidecar) is already
	// covered. Drop those children: otherwise their bytes are double-counted in the
	// total (a 25 GB .part counted once as orphan_partial and again inside its
	// empty_dir → an impossible "83 GB" on a 68 GB library) and applyFindings would
	// try to remove an already-gone path. Keeps the report and the freed-bytes honest.
	findings = dropFindingsUnderDirs(findings)

	return applyAndReturn(findings, roots, opts)
}

// applyAndReturn applies the findings when opts.Apply is set (else a zero summary)
// and returns the standard (findings, summary, nil) triple. Centralised so every
// exit path of ReconcileWithSummary applies identically.
func applyAndReturn(findings []Finding, roots []string, opts ReconcileOptions) ([]Finding, RemoveSummary, error) {
	if opts.Apply {
		return findings, applyFindings(findings, roots), nil
	}
	return findings, RemoveSummary{}, nil
}

// scanRoots walks every root once, classifying files and flagging media-named
// dirs, and returns the findings plus the set of already-flagged dirs (so the
// video-less pass skips them). A fatal WalkDir error on a root is returned; a
// per-entry traversal error is logged and skipped so one bad entry never aborts
// the sweep.
func scanRoots(roots []string, activePartials map[string]bool, opts ReconcileOptions) ([]Finding, map[string]bool, error) {
	sc := &scanContext{
		roots:          roots,
		activePartials: activePartials,
		opts:           opts,
		seenDirs:       map[string]bool{},
	}

	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			log.Printf("reconcile: skipping root %s (not accessible): %v", root, err)
			continue
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			return sc.classifyWalkEntry(walkEntry{path, d, err, root})
		})
		if err != nil {
			return sc.findings, sc.seenDirs, fmt.Errorf("walk %s: %w", root, err)
		}
	}
	return sc.findings, sc.seenDirs, nil
}

// scanContext carries the immutable scan config plus the accumulating findings /
// seen-dirs across every WalkDir callback, so classifyWalkEntry stays a method
// with a single argument (the per-entry walkEntry) rather than a 6-param func.
type scanContext struct {
	roots          []string
	activePartials map[string]bool
	opts           ReconcileOptions
	findings       []Finding
	seenDirs       map[string]bool
}

// walkEntry bundles a single WalkDir callback's inputs.
type walkEntry struct {
	path string
	d    os.DirEntry
	err  error
	root string
}

// classifyWalkEntry handles one WalkDir entry: log-and-skip a traversal error,
// flag a media-named dir, or classify a file into findings.
func (sc *scanContext) classifyWalkEntry(e walkEntry) error {
	if e.err != nil {
		// A traversal error on one entry must not abort the whole sweep; log and
		// keep going so the rest of the library still gets reconciled.
		log.Printf("reconcile: walk error at %s: %v", e.path, e.err)
		return nil
	}
	if e.d.IsDir() {
		// Directory whose NAME is a media file (e.g. "movie.mkv/") — a mis-created
		// folder from a bad organize. Flag the dir itself.
		if sc.opts.PruneEmptyDirs && e.path != e.root && isVideoFile(e.d.Name()) {
			sc.findings = append(sc.findings, dirFinding(e.path, KindMediaNamedDir,
				"directory name is a media filename (mis-created folder)"))
			sc.seenDirs[e.path] = true
			return filepath.SkipDir
		}
		return nil
	}
	if f := classifyFile(e.path, sc.roots, sc.activePartials, sc.opts); f != nil {
		sc.findings = append(sc.findings, *f)
	}
	return nil
}

// ReconcileSafe runs a reconcile with the given options and APPLIES only the safe
// (see safeKinds) categories — the deterministic ones fit for automatic execution
// from the daemon after each library scan. It returns the applied findings, the
// total bytes freed, and the RemoveSummary (so the daemon can log per-error
// guidance for anything it couldn't delete). opts.Apply is forced OFF for the
// report, then the safe subset is applied here. Nothing outside the configured
// roots is touched; every action is logged inside applyFindings.
func ReconcileSafe(paths ReconcilePaths, activePartials map[string]bool, opts ReconcileOptions) ([]Finding, int64, RemoveSummary, error) {
	opts.Apply = false // report first, decide what's safe, then apply the safe subset
	all, err := Reconcile(paths, activePartials, opts)
	if err != nil {
		return nil, 0, RemoveSummary{}, err
	}
	var safe []Finding
	for _, f := range all {
		if f.IsSafe() {
			safe = append(safe, f)
		}
	}
	summary := applyFindings(safe, paths.roots())
	// Report bytes ACTUALLY freed (summary.Freed), not the sum of everything we
	// intended to remove: a permission-denied / read-only mount leaves the file on
	// disk, and over-reporting "freed" would mask the failure the summary carries.
	return safe, summary.Freed, summary, nil
}

// classifyFile returns a Finding for a single file, or nil if it is a healthy
// video or something reconcile does not manage.
func classifyFile(path string, roots []string, activePartials map[string]bool, opts ReconcileOptions) *Finding {
	info, err := os.Stat(path)
	if err != nil {
		log.Printf("reconcile: stat %s failed: %v", path, err)
		return nil
	}
	ext := strings.ToLower(filepath.Ext(path))
	floor := opts.floor()

	switch {
	case isVideoFile(path):
		if opts.RemoveStubs && info.Size() < floor {
			return fileFinding(path, info, KindStubVideo,
				fmt.Sprintf("video is only %d bytes (< %d floor) — a download stub", info.Size(), floor))
		}
		return nil // a real video — never touch

	case partialExts[ext]:
		if !opts.RemoveOrphanPartials {
			return nil
		}
		if activePartials[filepath.Clean(path)] {
			return nil // an in-flight download owns it
		}
		return fileFinding(path, info, KindOrphanPartial,
			"partial download marker with no active task")

	case sidecarReconcileExts[ext]:
		if !opts.RemoveOrphanSubtitles {
			return nil
		}
		if sidecarHasOwner(path, floor) {
			return nil // belongs to a video reachable from its (or its parent's) dir
		}
		return fileFinding(path, info, KindOrphanSidecar,
			"sidecar with no owning video (checked its dir and, for .unarr sidecars, the parent release dir)")
	}
	return nil
}

// sidecarHasOwner decides whether a sidecar still belongs to a video. A sidecar is
// owned if a real video (>= floor) lives in ITS OWN directory OR — when the sidecar
// sits inside a per-track ".unarr" cache dir — in the PARENT release directory.
//
// The .unarr case is why the naive "video in same dir" check produced 107 false
// orphans in the field: the scanner extracts per-track WebVTT/thumbnails into
// "<release>/.unarr/*.vtt", where there is no video beside them. Those are only
// orphaned when the release itself no longer holds the video (it was moved to TV
// Shows and just .nfo/.txt/.unarr remain).
func sidecarHasOwner(path string, floor int64) bool {
	dir := filepath.Dir(path)
	if dirHasImmediateVideo(dir, floor) {
		return true
	}
	// Per-track sidecars live in a ".unarr" cache dir — fall back to the parent
	// release dir, which is where the actual media file lives.
	if strings.EqualFold(filepath.Base(dir), ".unarr") {
		if dirHasImmediateVideo(filepath.Dir(dir), floor) {
			return true
		}
	}
	return false
}

// dirHasImmediateVideo reports whether a real video (>= floor) sits directly in dir
// (non-recursive). On a read error it returns true (conservative: don't flag a
// sidecar we can't prove is orphaned).
func dirHasImmediateVideo(dir string, floor int64) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("reconcile: read %s failed, treating sidecars as attached: %v", dir, err)
		return true
	}
	for _, e := range entries {
		if e.IsDir() || !isVideoFile(e.Name()) {
			continue
		}
		if info, err := e.Info(); err == nil && info.Size() >= floor {
			return true
		}
	}
	return false
}

// findVideolessDirs returns dirs (strictly inside a root, never a CONFIGURED dir
// itself, never already-flagged) that contain no real video anywhere beneath them.
//
// "Configured dir" means any of the reconcile roots (download/movies/tv). Guarding
// only `path == root` of the current walk is NOT enough: when the download dir is
// the PARENT of the movies/tv dirs (e.g. download=/media, movies=/media/Movies),
// the movies/tv dirs are ordinary sub-entries of the download walk, so an empty
// library would have its Movies/ and TV Shows/ target dirs deleted — a real bug the
// e2e caught (the daemon auto-sweep removed both on a fresh empty library). These
// dirs are organize TARGETS and must survive even when momentarily empty.
func findVideolessDirs(roots []string, skip map[string]bool, floor int64) []Finding {
	protected := map[string]bool{}
	for _, r := range roots {
		protected[filepath.Clean(r)] = true
	}
	var out []Finding
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() || path == root || protected[filepath.Clean(path)] || skip[path] {
				return nil
			}
			// A ".unarr" cache dir legitimately holds only sidecars for the video in
			// its PARENT release dir — it is not a videoless orphan when that parent
			// still has the video. (Orphaned .unarr sidecars are handled per-file by
			// sidecarHasOwner, which removes them and lets the empty .unarr get pruned.)
			if strings.EqualFold(d.Name(), ".unarr") && dirHasRealVideo(filepath.Dir(path), floor) {
				return filepath.SkipDir
			}
			if dirHasRealVideo(path, floor) {
				return nil
			}
			out = append(out, dirFinding(path, KindEmptyDir,
				"directory contains no valid video (empty or only junk/stubs)"))
			skip[path] = true
			return filepath.SkipDir // its (also video-less) children are covered by removing this
		})
	}
	return out
}

// dirHasRealVideo reports whether a real video (>= floor) exists anywhere under dir.
func dirHasRealVideo(dir string, floor int64) bool {
	found := false
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if isVideoFile(path) {
			if info, statErr := os.Stat(path); statErr == nil && info.Size() >= floor {
				found = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found
}

// isVideoFile mirrors engine.isVideoFile using the library's videoExts set (from
// scanner.go). Kept here so reconcile does not depend on the engine package.
func isVideoFile(name string) bool {
	return videoExts[strings.ToLower(filepath.Ext(name))]
}
