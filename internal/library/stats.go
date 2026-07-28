package library

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

// LibraryStats is the serializable result of `unarr library stats`. It is a pure
// DRY-RUN report (nothing on disk is modified) split into three blocks:
// composition (counts + real on-disk bytes), health (reconcile findings grouped
// by kind), and quality (resolution/codec/HDR breakdown). Every byte figure is
// REAL on-disk usage via diskUsage — never apparent size.
type LibraryStats struct {
	// Roots lists the configured directories that were actually walked (empty
	// entries and unreadable roots are skipped).
	Roots       []string         `json:"roots"`
	Composition CompositionStats `json:"composition"`
	Health      HealthStats      `json:"health"`
	Quality     QualityStats     `json:"quality"`
}

// CompositionStats counts titles and their real on-disk footprint.
type CompositionStats struct {
	Movies     int   `json:"movies"`     // video files in the Movies dir
	Shows      int   `json:"shows"`      // top-level show dirs in the TV dir
	Episodes   int   `json:"episodes"`   // episode video files in the TV dir
	Seasons    int   `json:"seasons"`    // distinct (show, season) pairs in the TV dir
	MovieBytes int64 `json:"movieBytes"` // real on-disk bytes assigned to Movies
	TVBytes    int64 `json:"tvBytes"`    // real on-disk bytes assigned to TV Shows
	// DownloadBytes is the real on-disk bytes of videos in the download dir but
	// OUTSIDE Movies/TV — the raw, unorganized remainder. When the download dir is
	// an ancestor of Movies/TV (the common layout), those nested files are counted
	// under Movies/TV, NOT here, so the three categories stay disjoint.
	DownloadBytes int64 `json:"downloadBytes"`
	TotalBytes    int64 `json:"totalBytes"` // MovieBytes + TVBytes + DownloadBytes (disjoint → matches du)
	// AvgMovieBytes / AvgEpisodeBytes are the mean real on-disk size per title
	// (0 when the count is 0), so a table can show "average per title" directly.
	AvgMovieBytes   int64 `json:"avgMovieBytes"`
	AvgEpisodeBytes int64 `json:"avgEpisodeBytes"`
}

// HealthCategory is one reconcile kind rolled up to a count + real bytes.
type HealthCategory struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
	Bytes int64  `json:"bytes"` // real on-disk bytes (Finding.Bytes)
}

// HealthStats is the dry-run reconcile result grouped by kind. Reclaimable is the
// sum of every Finding.Bytes — the real space `unarr library clean --apply` would
// free.
type HealthStats struct {
	Categories       []HealthCategory `json:"categories"`
	TotalFindings    int              `json:"totalFindings"`
	ReclaimableBytes int64            `json:"reclaimableBytes"`
}

// QualityStats is the resolution / codec / HDR breakdown across every video.
type QualityStats struct {
	// ByResolution keys off ResolveResolution: "2160p"/"1080p"/"720p"/"480p", plus
	// "SD" (a resolved-but-below-480p / unknown-dimension video) and "unknown" (no
	// mediainfo at all).
	ByResolution map[string]int `json:"byResolution"`
	// ByCodec buckets the raw video codec into "h265"/"h264"/"av1"/"other"/"unknown".
	ByCodec map[string]int `json:"byCodec"`
	HDR     int            `json:"hdr"`     // videos with any HDR flag (HDR10/DV/HLG/…)
	SDR     int            `json:"sdr"`     // videos probed with no HDR flag
	Unknown int            `json:"unknown"` // videos with no usable mediainfo
	Total   int            `json:"total"`   // videos considered
}

// MediaProber extracts media info for one file. Production uses
// mediainfo.ExtractMediaInfo; tests inject a stub so the quality block never
// depends on a real ffprobe being installed on the runner.
type MediaProber func(ctx context.Context, filePath string) (*mediainfo.VideoInfo, error)

// StatsOptions configures ComputeStats.
type StatsOptions struct {
	// Paths confines the whole report to these roots (download/movies/tv), exactly
	// like reconcile — nothing outside is walked.
	Paths ReconcilePaths
	// Reconcile drives the dry-run health pass. Apply is forced OFF regardless of
	// what the caller sets, so ComputeStats can NEVER modify anything on disk.
	Reconcile ReconcileOptions
	// Workers bounds the concurrent probes for the quality pass (default 8), the
	// same pool shape the scanner uses.
	Workers int
	// Probe extracts a video's mediainfo. When nil, ComputeStats builds the real
	// ffprobe-backed prober (resolving ffprobe from FFprobePath); a file that fails
	// to probe is counted as "unknown" and never aborts the pass.
	Probe MediaProber
	// FFprobePath is passed to the default prober when Probe is nil ("" auto-resolves).
	FFprobePath string
	// OnProgress, when set, is called after each probed video with (done, total).
	OnProgress func(done, total int)
}

// ComputeStats builds the full LibraryStats for the configured roots. It is a
// pure reader: opts.Reconcile.Apply is forced false and no path is ever modified.
// The three blocks are independent, so a failure to build the ffprobe prober only
// degrades the quality block to "unknown" — composition and health still report.
func ComputeStats(ctx context.Context, opts StatsOptions) (*LibraryStats, error) {
	roots := opts.Paths.roots()
	stats := &LibraryStats{Roots: roots}

	stats.Composition = computeComposition(opts.Paths)

	health, err := computeHealth(opts.Paths, opts.Reconcile)
	if err != nil {
		return nil, err
	}
	stats.Health = health

	stats.Quality = computeQuality(ctx, opts)
	return stats, nil
}

// categoryBuckets accumulates disjoint per-category counts + real on-disk bytes.
// Each video file lands in EXACTLY ONE bucket (its most-specific containing root),
// so the three buckets never overlap and their sum equals `du` of the union.
type categoryBuckets struct {
	movieCount, moviesBytes int64
	tvBytes                 int64
	otherCount, otherBytes  int64 // download dir MINUS Movies/TV (raw / unorganized)
}

// computeComposition assigns every real video (>= floor) under the configured
// roots to exactly one category — the DEEPEST root that contains it — so bytes are
// counted ONCE even when the download dir is an ANCESTOR of Movies/TV (the common
// layout: download=/Media, movies=/Media/Movies). Movies/TV get their own bytes;
// "other" is the download dir minus Movies/TV (raw, unsorted releases). The show /
// season / episode structure is derived from a separate TV walk (counts only —
// never summed into a byte total, so no overlap concern). Confined to the roots.
func computeComposition(paths ReconcilePaths) CompositionStats {
	var c CompositionStats

	buckets := tallyByCategory(paths)
	c.Movies = int(buckets.movieCount)
	c.MovieBytes = buckets.moviesBytes
	c.TVBytes = buckets.tvBytes
	c.DownloadBytes = buckets.otherBytes // ONLY the unorganized remainder now
	c.TotalBytes = c.MovieBytes + c.TVBytes + c.DownloadBytes

	if paths.TVShowsDir != "" {
		c.Shows, c.Episodes, c.Seasons, _ = walkTVShows(filepath.Clean(paths.TVShowsDir))
	}
	if c.Movies > 0 {
		c.AvgMovieBytes = c.MovieBytes / int64(c.Movies)
	}
	if c.Episodes > 0 {
		c.AvgEpisodeBytes = c.TVBytes / int64(c.Episodes)
	}
	return c
}

// categoryRoot is a configured root tagged with the category any video under it
// belongs to. Kinds: "movie", "tv", "other" (download).
type categoryRoot struct {
	root string
	kind string
}

// categoryRoots orders the configured category roots by specificity — DEEPEST
// (most path segments) first — so a single-assignment walk hands each file to the
// narrowest category that contains it. Empty roots are dropped; a root nested in
// another still gets its own kind so the deepest wins.
func categoryRoots(paths ReconcilePaths) []categoryRoot {
	var raw []categoryRoot
	add := func(dir, kind string) {
		if dir != "" {
			raw = append(raw, categoryRoot{filepath.Clean(dir), kind})
		}
	}
	add(paths.MoviesDir, "movie")
	add(paths.TVShowsDir, "tv")
	add(paths.DownloadDir, "other")

	// Deepest path first (more separators = more specific). Exact duplicates are
	// collapsed by the walk's `seen` set; ties keep insertion order (movie, tv, other).
	sort.SliceStable(raw, func(i, j int) bool {
		return pathDepth(raw[i].root) > pathDepth(raw[j].root)
	})
	return raw
}

// pathDepth counts path segments (separator count) so deeper paths sort first.
// filepath.Separator keeps it correct on Windows (backslash) and POSIX (slash).
func pathDepth(p string) int {
	return strings.Count(p, string(filepath.Separator))
}

// tallyByCategory walks the OUTERMOST roots once and assigns each real video to the
// deepest category root that contains it, deduplicating files reachable from more
// than one overlapping root. This is the core of the disjoint-bytes fix.
func tallyByCategory(paths ReconcilePaths) categoryBuckets {
	roots := categoryRoots(paths)
	var b categoryBuckets
	seen := map[string]bool{}

	// Walk from every root, but each file is counted once (via `seen`) and assigned
	// to the deepest owning root (roots are pre-sorted deepest-first, so the first
	// containing root in that order is the most specific).
	for _, r := range roots {
		_ = filepath.WalkDir(r.root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !isVideoFile(path) {
				return nil
			}
			clean := filepath.Clean(path)
			if seen[clean] {
				return nil
			}
			info, statErr := os.Stat(path)
			if statErr != nil || info.Size() < MinPlausibleVideoBytes {
				return nil
			}
			seen[clean] = true
			assignToBucket(&b, kindForPath(clean, roots), diskUsage(info))
			return nil
		})
	}
	return b
}

// kindForPath returns the kind of the DEEPEST root (roots are deepest-first) that
// contains path — the file's most-specific category. Falls back to "other".
func kindForPath(path string, roots []categoryRoot) string {
	for _, r := range roots {
		if isUnderRootOrEqual(r.root, path) {
			return r.kind
		}
	}
	return "other"
}

// assignToBucket adds one file's real bytes (and, for movies, its count) to its
// category bucket. TV/other episode counts come from their own walks.
func assignToBucket(b *categoryBuckets, kind string, bytes int64) {
	switch kind {
	case "movie":
		b.movieCount++
		b.moviesBytes += bytes
	case "tv":
		b.tvBytes += bytes
	default: // "other" — download dir minus Movies/TV
		b.otherCount++
		b.otherBytes += bytes
	}
}

// isUnderRootOrEqual reports whether path is root itself or lives inside it, using
// filepath.Rel (not a string prefix) so "/media/tv" doesn't swallow "/media/tv-x".
func isUnderRootOrEqual(root, path string) bool {
	if root == "" {
		return false
	}
	if filepath.Clean(root) == filepath.Clean(path) {
		return true
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// walkVideos returns the count and total real on-disk bytes of every video file
// (>= the plausibility floor) anywhere under root. diskUsage gives the real bytes.
// Used by the TV structure walk (single-root, no overlap) — the disjoint per-
// category byte tally goes through tallyByCategory.
func walkVideos(root string) (count int, bytes int64) {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isVideoFile(path) {
			return nil
		}
		info, statErr := os.Stat(path)
		if statErr != nil || info.Size() < MinPlausibleVideoBytes {
			return nil
		}
		count++
		bytes += diskUsage(info)
		return nil
	})
	return count, bytes
}

// walkTVShows tallies shows (top-level dirs under root), episodes (video files),
// seasons (distinct show+season pairs), and real on-disk bytes. A show with a
// video directly in its dir still counts as one show; the season is derived from
// each filename via ParseSeasonEpisode (0 → an "unnumbered" season bucket).
func walkTVShows(root string) (shows, episodes, seasons int, bytes int64) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, 0, 0, 0
	}
	seasonSet := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		showDir := filepath.Join(root, e.Name())
		epCount, showBytes := walkVideos(showDir)
		if epCount == 0 {
			continue // a dir with no real video is not a show (empty/junk)
		}
		shows++
		episodes += epCount
		bytes += showBytes
		collectSeasons(showDir, e.Name(), seasonSet)
	}
	return shows, episodes, len(seasonSet), bytes
}

// collectSeasons records each distinct (show, season) key for the videos under
// showDir into set. The season number is parsed from the filename; season 0
// (unparseable) still forms a distinct bucket so a flat show counts as one season.
func collectSeasons(showDir, show string, set map[string]bool) {
	_ = filepath.WalkDir(showDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isVideoFile(path) {
			return nil
		}
		info, statErr := os.Stat(path)
		if statErr != nil || info.Size() < MinPlausibleVideoBytes {
			return nil
		}
		season, _ := ParseSeasonEpisode(d.Name())
		set[show+"#"+itoa(season)] = true
		return nil
	})
}

// computeHealth runs the reconcile in DRY-RUN (Apply forced off) and rolls the
// findings up by kind. Reclaimable is the sum of Finding.Bytes (real on-disk).
func computeHealth(paths ReconcilePaths, opts ReconcileOptions) (HealthStats, error) {
	opts.Apply = false // NEVER modify anything from stats
	// Stats paints the FULL health picture, so evaluate the corrupt-video category
	// even though it is opt-in for actual removal — the user wants to KNOW how many
	// suspect (zero-content) videos exist, whether or not they've enabled cleanup.
	opts.RemoveCorruptVideos = true
	findings, _, err := ReconcileWithSummary(paths, nil, opts)
	if err != nil {
		return HealthStats{}, err
	}
	byKind := map[FindingKind]*HealthCategory{}
	var order []FindingKind
	var total int64
	for _, f := range findings {
		cat, ok := byKind[f.Kind]
		if !ok {
			cat = &HealthCategory{Kind: string(f.Kind)}
			byKind[f.Kind] = cat
			order = append(order, f.Kind)
		}
		cat.Count++
		cat.Bytes += f.Bytes
		total += f.Bytes
	}
	cats := make([]HealthCategory, 0, len(order))
	for _, k := range order {
		cats = append(cats, *byKind[k])
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i].Bytes > cats[j].Bytes })
	return HealthStats{
		Categories:       cats,
		TotalFindings:    len(findings),
		ReclaimableBytes: total,
	}, nil
}

// The stats QUALITY pass (computeQuality, probeAll, defaultProber, …) lives in
// stats_quality.go.

// itoa is a tiny non-allocating-ish int→string for the season-set key (avoids a
// strconv import for a one-liner).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
