package library

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

// seedStatsLibrary builds a tmp library with a Movies dir (2 films: one tagged
// 1080p, one 2160p), a TV dir (1 show, 2 seasons, 3 episodes), and some junk in
// the download dir (1 stub video + 1 duplicate pair). It returns the paths plus a
// deterministic prober that reports resolution from the filename tag, so the
// quality assertions never depend on a real ffprobe being installed.
func seedStatsLibrary(t *testing.T) (ReconcilePaths, MediaProber) {
	t.Helper()
	root := t.TempDir()
	movies := filepath.Join(root, "Movies")
	tv := filepath.Join(root, "TV Shows")
	dl := filepath.Join(root, "downloads")

	const real = 2 * 1024 * 1024 // >= 1 MiB floor

	// Every real video gets a DISTINCT head marker so only the intended duplicate
	// pair is byte-identical — otherwise same-size zero-filled files collide as
	// cross-directory duplicates and inflate the finding count.
	// Movies: one 1080p, one 2160p.
	writeVideoWithMarker(t, filepath.Join(movies, "Alpha (2020) 1080p x264.mkv"), real, 0x01)
	writeVideoWithMarker(t, filepath.Join(movies, "Beta (2021) 2160p x265 HDR10.mkv"), real+1024, 0x02)

	// TV: one show, 2 seasons (S01 x2, S02 x1) = 3 episodes.
	show := filepath.Join(tv, "My Show")
	writeVideoWithMarker(t, filepath.Join(show, "My Show S01E01 1080p.mkv"), real, 0x03)
	writeVideoWithMarker(t, filepath.Join(show, "My Show S01E02 1080p.mkv"), real, 0x04)
	writeVideoWithMarker(t, filepath.Join(show, "My Show S02E01 720p.mkv"), real, 0x05)

	// Junk in downloads: a stub (< floor) and a byte-identical duplicate pair.
	writeSized(t, filepath.Join(dl, "stub.mkv"), 512) // < 1 MiB → stub finding
	writeVideoWithMarker(t, filepath.Join(dl, "dupdir", "Gamma 1080p.mkv"), real, 0x11)
	writeVideoWithMarker(t, filepath.Join(dl, "dupdir", "Gamma 1080p copy.mkv"), real, 0x11)

	paths := ReconcilePaths{DownloadDir: dl, MoviesDir: movies, TVShowsDir: tv}

	// Deterministic prober: infer resolution/codec/HDR from the filename tag.
	probe := func(_ context.Context, filePath string) (*mediainfo.VideoInfo, error) {
		return videoInfoFromName(filepath.Base(filePath)), nil
	}
	return paths, probe
}

// videoInfoFromName fabricates a VideoInfo from the filename's quality tag so the
// test's quality assertions are deterministic without ffprobe.
func videoInfoFromName(name string) *mediainfo.VideoInfo {
	vi := &mediainfo.VideoInfo{Codec: "h264", Width: 1920, Height: 1080}
	switch {
	case strings.Contains(name, "2160p"):
		vi.Width, vi.Height, vi.Codec = 3840, 2160, "hevc"
	case strings.Contains(name, "720p"):
		vi.Width, vi.Height = 1280, 720
	}
	if strings.Contains(name, "HDR10") {
		vi.HDR = "HDR10"
	}
	return vi
}

func TestComputeStatsComposition(t *testing.T) {
	paths, probe := seedStatsLibrary(t)

	stats, err := ComputeStats(context.Background(), StatsOptions{
		Paths:     paths,
		Reconcile: DefaultReconcileOptions(),
		Probe:     probe,
	})
	if err != nil {
		t.Fatalf("ComputeStats: %v", err)
	}

	c := stats.Composition
	if c.Movies != 2 {
		t.Errorf("Movies = %d, want 2", c.Movies)
	}
	if c.Shows != 1 {
		t.Errorf("Shows = %d, want 1", c.Shows)
	}
	if c.Episodes != 3 {
		t.Errorf("Episodes = %d, want 3", c.Episodes)
	}
	if c.Seasons != 2 {
		t.Errorf("Seasons = %d, want 2", c.Seasons)
	}
	// Real on-disk bytes: 2 movies of ~2 MiB each → MovieBytes must be well above
	// the floor and TVBytes above it too (3 episodes). Exact block counts are
	// filesystem-dependent, so assert lower bounds + internal consistency.
	if c.MovieBytes < 2*MinPlausibleVideoBytes {
		t.Errorf("MovieBytes = %d, want >= %d", c.MovieBytes, 2*MinPlausibleVideoBytes)
	}
	if c.TVBytes < 3*MinPlausibleVideoBytes {
		t.Errorf("TVBytes = %d, want >= %d", c.TVBytes, 3*MinPlausibleVideoBytes)
	}
	if c.TotalBytes != c.MovieBytes+c.TVBytes+c.DownloadBytes {
		t.Errorf("TotalBytes = %d, want sum %d", c.TotalBytes,
			c.MovieBytes+c.TVBytes+c.DownloadBytes)
	}
	if c.AvgMovieBytes != c.MovieBytes/2 {
		t.Errorf("AvgMovieBytes = %d, want %d", c.AvgMovieBytes, c.MovieBytes/2)
	}
	if c.AvgEpisodeBytes != c.TVBytes/3 {
		t.Errorf("AvgEpisodeBytes = %d, want %d", c.AvgEpisodeBytes, c.TVBytes/3)
	}
}

func TestComputeStatsHealth(t *testing.T) {
	paths, probe := seedStatsLibrary(t)

	stats, err := ComputeStats(context.Background(), StatsOptions{
		Paths:     paths,
		Reconcile: DefaultReconcileOptions(),
		Probe:     probe,
	})
	if err != nil {
		t.Fatalf("ComputeStats: %v", err)
	}

	byKind := map[string]HealthCategory{}
	for _, cat := range stats.Health.Categories {
		byKind[cat.Kind] = cat
	}
	if stub := byKind[string(KindStubVideo)]; stub.Count != 1 {
		t.Errorf("stub_video count = %d, want 1", stub.Count)
	}
	if dup := byKind[string(KindDuplicate)]; dup.Count != 1 {
		t.Errorf("duplicate_video count = %d, want 1", dup.Count)
	}
	if stats.Health.TotalFindings < 2 {
		t.Errorf("TotalFindings = %d, want >= 2 (stub + duplicate)", stats.Health.TotalFindings)
	}
	if stats.Health.ReclaimableBytes <= 0 {
		t.Errorf("ReclaimableBytes = %d, want > 0", stats.Health.ReclaimableBytes)
	}

	// A stats run must NEVER modify disk: the stub and both dup copies survive.
	for _, p := range []string{
		filepath.Join(paths.DownloadDir, "stub.mkv"),
		filepath.Join(paths.DownloadDir, "dupdir", "Gamma 1080p.mkv"),
		filepath.Join(paths.DownloadDir, "dupdir", "Gamma 1080p copy.mkv"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("stats modified disk — %s missing: %v", p, err)
		}
	}
}

func TestComputeStatsQuality(t *testing.T) {
	paths, probe := seedStatsLibrary(t)

	stats, err := ComputeStats(context.Background(), StatsOptions{
		Paths:     paths,
		Reconcile: DefaultReconcileOptions(),
		Probe:     probe,
	})
	if err != nil {
		t.Fatalf("ComputeStats: %v", err)
	}

	q := stats.Quality
	// Videos considered: 2 movies + 3 episodes + 2 duplicates (stub is < floor and
	// excluded from the quality walk) = 7.
	if q.Total != 7 {
		t.Fatalf("quality Total = %d, want 7", q.Total)
	}
	// One 2160p (Beta), one 720p (S02E01), the rest 1080p (Alpha + 2 S01 eps + 2 dups).
	if q.ByResolution["2160p"] != 1 {
		t.Errorf("2160p = %d, want 1", q.ByResolution["2160p"])
	}
	if q.ByResolution["720p"] != 1 {
		t.Errorf("720p = %d, want 1", q.ByResolution["720p"])
	}
	if q.ByResolution["1080p"] != 5 {
		t.Errorf("1080p = %d, want 5", q.ByResolution["1080p"])
	}
	// Codec: Beta is hevc → h265; everything else h264.
	if q.ByCodec["h265"] != 1 {
		t.Errorf("h265 = %d, want 1", q.ByCodec["h265"])
	}
	if q.ByCodec["h264"] != 6 {
		t.Errorf("h264 = %d, want 6", q.ByCodec["h264"])
	}
	// Only Beta is HDR.
	if q.HDR != 1 {
		t.Errorf("HDR = %d, want 1", q.HDR)
	}
	if q.SDR != 6 {
		t.Errorf("SDR = %d, want 6", q.SDR)
	}
}

// TestComputeStatsUnknownProber verifies a prober that always errors (e.g. ffprobe
// absent) degrades every video to "unknown" instead of aborting.
func TestComputeStatsUnknownProber(t *testing.T) {
	paths, _ := seedStatsLibrary(t)
	failing := func(context.Context, string) (*mediainfo.VideoInfo, error) {
		return nil, errNoVideoStream
	}
	stats, err := ComputeStats(context.Background(), StatsOptions{
		Paths:     paths,
		Reconcile: DefaultReconcileOptions(),
		Probe:     failing,
	})
	if err != nil {
		t.Fatalf("ComputeStats: %v", err)
	}
	if stats.Quality.Unknown != stats.Quality.Total {
		t.Errorf("Unknown = %d, want all %d", stats.Quality.Unknown, stats.Quality.Total)
	}
}

// TestComputeStatsJSON round-trips the struct through JSON (the --json path) and
// checks the key fields survive.
func TestComputeStatsJSON(t *testing.T) {
	paths, probe := seedStatsLibrary(t)
	stats, err := ComputeStats(context.Background(), StatsOptions{
		Paths:     paths,
		Reconcile: DefaultReconcileOptions(),
		Probe:     probe,
	})
	if err != nil {
		t.Fatalf("ComputeStats: %v", err)
	}

	blob, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got LibraryStats
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Composition.Movies != 2 {
		t.Errorf("json Composition.Movies = %d, want 2", got.Composition.Movies)
	}
	if got.Composition.Episodes != 3 {
		t.Errorf("json Composition.Episodes = %d, want 3", got.Composition.Episodes)
	}
	if got.Quality.ByResolution["2160p"] != 1 {
		t.Errorf("json 2160p = %d, want 1", got.Quality.ByResolution["2160p"])
	}
	if got.Health.TotalFindings != stats.Health.TotalFindings {
		t.Errorf("json TotalFindings = %d, want %d", got.Health.TotalFindings, stats.Health.TotalFindings)
	}
}

// TestComputeStatsCompositionNoDoubleCount is the regression for the real-prod
// bug: when the DOWNLOAD DIR is an ANCESTOR of Movies/TV (download=/Media,
// movies=/Media/Movies, tv=/Media/TV Shows), the old code summed the whole
// download tree into "Downloads" AND again into Movies/TV, so Total was 2-3× du.
// Now each file is assigned to its most-specific category exactly once:
// Movies = only Movies, TV = only TV, Downloads = only the raw remainder, and
// Total == the sum of every distinct file (== du).
func TestComputeStatsCompositionNoDoubleCount(t *testing.T) {
	root := t.TempDir() // the download dir IS the tmp root
	movies := filepath.Join(root, "Movies")
	tv := filepath.Join(root, "TV Shows")

	const real = 2 * 1024 * 1024
	// Distinct markers so nothing collides as a cross-dir duplicate.
	writeVideoWithMarker(t, filepath.Join(movies, "Alpha (2020) 1080p.mkv"), real, 0x01)
	writeVideoWithMarker(t, filepath.Join(movies, "Beta (2021) 1080p.mkv"), real, 0x02)
	writeVideoWithMarker(t, filepath.Join(tv, "My Show", "My Show S01E01 1080p.mkv"), real, 0x03)
	writeVideoWithMarker(t, filepath.Join(tv, "My Show", "My Show S01E02 1080p.mkv"), real, 0x04)
	// A raw, unorganized release directly under the download dir (outside Movies/TV).
	writeVideoWithMarker(t, filepath.Join(root, "raw_folder", "Gamma 1080p.mkv"), real, 0x05)

	// download dir = the ancestor tmp root; movies/tv nested inside it.
	paths := ReconcilePaths{DownloadDir: root, MoviesDir: movies, TVShowsDir: tv}

	stats, err := ComputeStats(context.Background(), StatsOptions{
		Paths:     paths,
		Reconcile: DefaultReconcileOptions(),
		Probe:     func(context.Context, string) (*mediainfo.VideoInfo, error) { return nil, errNoVideoStream },
	})
	if err != nil {
		t.Fatalf("ComputeStats: %v", err)
	}
	c := stats.Composition

	// Independent ground truth: real on-disk bytes of EVERY video, counted once.
	wantMovies := realBytes(t, movies)                         // 2 files
	wantTV := realBytes(t, tv)                                 // 2 files
	wantRaw := realBytes(t, filepath.Join(root, "raw_folder")) // 1 file
	wantTotal := wantMovies + wantTV + wantRaw                 // == du of the union

	if c.MovieBytes != wantMovies {
		t.Errorf("MovieBytes = %d, want %d (only Movies)", c.MovieBytes, wantMovies)
	}
	if c.TVBytes != wantTV {
		t.Errorf("TVBytes = %d, want %d (only TV)", c.TVBytes, wantTV)
	}
	// Downloads is now ONLY the raw remainder — NOT the whole /Media tree.
	if c.DownloadBytes != wantRaw {
		t.Errorf("DownloadBytes = %d, want %d (only raw_folder, not Movies/TV)", c.DownloadBytes, wantRaw)
	}
	if c.TotalBytes != wantTotal {
		t.Errorf("TotalBytes = %d, want %d (disjoint sum == du)", c.TotalBytes, wantTotal)
	}
	// The old bug would have made Total ≈ 2× the raw+nested tree. Guard it: Total
	// must not exceed the true union by even one file's worth.
	if c.TotalBytes > wantTotal {
		t.Errorf("TotalBytes %d overcounts the union %d — overlap double-counted", c.TotalBytes, wantTotal)
	}
	if c.Movies != 2 {
		t.Errorf("Movies = %d, want 2", c.Movies)
	}
	if c.Episodes != 2 {
		t.Errorf("Episodes = %d, want 2", c.Episodes)
	}
}

// realBytes sums the real on-disk usage (diskUsage) of every real video (>= floor)
// under dir — the same measure computeComposition uses, so the test's ground truth
// matches the implementation's byte semantics on any filesystem.
func realBytes(t *testing.T, dir string) int64 {
	t.Helper()
	_, bytes := walkVideos(dir)
	return bytes
}

// TestComputeStatsCountsCorruptVideos verifies stats reports suspect zero-content
// videos even though RemoveCorruptVideos is OFF in the passed options: computeHealth
// force-enables the category so the user always SEES the count (#5). Nothing is
// modified — the corrupt file survives.
func TestComputeStatsCountsCorruptVideos(t *testing.T) {
	root := t.TempDir()
	dl := filepath.Join(root, "downloads")
	corrupt := filepath.Join(dl, "S01E03 (2).mkv")
	// Right size (> 2 MiB so head/tail are sampled), first 1 MiB all zero.
	writeHeadMidTail(t, corrupt, 3*fpChunk, 0x00, 0xAB, 0xCD)

	probe := func(_ context.Context, filePath string) (*mediainfo.VideoInfo, error) {
		return videoInfoFromName(filepath.Base(filePath)), nil
	}

	stats, err := ComputeStats(context.Background(), StatsOptions{
		Paths:     ReconcilePaths{DownloadDir: dl},
		Reconcile: DefaultReconcileOptions(), // RemoveCorruptVideos is FALSE here
		Probe:     probe,
	})
	if err != nil {
		t.Fatalf("ComputeStats: %v", err)
	}

	var corruptCount int
	for _, cat := range stats.Health.Categories {
		if cat.Kind == string(KindCorruptVideo) {
			corruptCount = cat.Count
		}
	}
	if corruptCount != 1 {
		t.Errorf("corrupt_video count = %d, want 1 (stats must surface suspect videos)", corruptCount)
	}
	// Pure dry-run: the corrupt file is NOT removed.
	if _, err := os.Stat(corrupt); err != nil {
		t.Errorf("stats modified disk — corrupt file removed: %v", err)
	}
}
