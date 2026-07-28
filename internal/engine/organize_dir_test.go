package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOrganizeMultiFileReleaseNormalizes (RC-5) asserts that a directory release
// gets normalized: the principal (largest) video lands in the library under the
// canonical name with its subtitle, while sample clips and .nfo junk are left
// behind (the source dir is cleaned up). Previously the whole raw folder was moved
// into the library untouched.
func TestOrganizeMultiFileReleaseNormalizes(t *testing.T) {
	tmp := t.TempDir()
	tvDir := filepath.Join(tmp, "TV")
	downloadDir := filepath.Join(tmp, "downloads")

	// Build a realistic release folder inside the download dir.
	releaseDir := filepath.Join(downloadDir, "Show.S01E02.1080p.WEB.x265-GRP")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Principal video: large.
	mainVideo := filepath.Join(releaseDir, "Show S01E02.mkv")
	if err := os.WriteFile(mainVideo, make([]byte, 5*1024*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	// Its subtitle (same basename).
	if err := os.WriteFile(filepath.Join(releaseDir, "Show S01E02.es.srt"), []byte("sub"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A small sample video that must NOT be treated as the feature.
	if err := os.WriteFile(filepath.Join(releaseDir, "sample.mkv"), make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	// Junk metadata.
	if err := os.WriteFile(filepath.Join(releaseDir, "release.nfo"), []byte("nfo"), 0o644); err != nil {
		t.Fatal(err)
	}

	season := 1
	episode := 2
	cfg := OrganizeConfig{Enabled: true, TVShowsDir: tvDir, OutputDir: downloadDir}
	result := &Result{
		FilePath: releaseDir,
		FileName: filepath.Base(releaseDir),
		Method:   MethodTorrent,
	}
	task := &Task{
		ContentType:  "show",
		ContentTitle: "Show",
		Season:       &season,
		Episode:      &episode,
		Title:        "Show.S01E02.1080p.WEB.x265-GRP",
	}

	dest, err := organize(result, task, cfg)
	if err != nil {
		t.Fatalf("organize failed: %v", err)
	}

	// Principal video landed normalized in the season folder.
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("destination video not found: %v", err)
	}
	if info.IsDir() {
		t.Fatalf("destination should be a file, got a directory: %s", dest)
	}
	if info.Size() != 5*1024*1024 {
		t.Errorf("destination is not the principal video (size %d, want %d) — sample may have won", info.Size(), 5*1024*1024)
	}
	wantName := "Show - S01E02.mkv"
	if filepath.Base(dest) != wantName {
		t.Errorf("destination name = %q, want %q", filepath.Base(dest), wantName)
	}

	// Subtitle followed the video, renamed to match.
	subDest := filepath.Join(filepath.Dir(dest), "Show - S01E02.es.srt")
	if _, err := os.Stat(subDest); err != nil {
		t.Errorf("subtitle did not follow the video to %s: %v", subDest, err)
	}

	// sample.mkv and .nfo did not contaminate the library.
	seasonDir := filepath.Dir(dest)
	if _, err := os.Stat(filepath.Join(seasonDir, "sample.mkv")); !os.IsNotExist(err) {
		t.Errorf("sample.mkv leaked into the library dir")
	}
	if _, err := os.Stat(filepath.Join(seasonDir, "release.nfo")); !os.IsNotExist(err) {
		t.Errorf("release.nfo leaked into the library dir")
	}

	// The source release dir was cleaned up (only junk + sample remained).
	if _, err := os.Stat(releaseDir); !os.IsNotExist(err) {
		t.Errorf("source release dir should have been removed, still exists: %v", err)
	}
}

// TestOrganizeMultiFileReleaseNoVideoFallsBack asserts the safety fallback: a dir
// with no video inside is moved as-is (nothing lost) rather than dropped.
func TestOrganizeMultiFileReleaseNoVideoFallsBack(t *testing.T) {
	tmp := t.TempDir()
	moviesDir := filepath.Join(tmp, "Movies")
	downloadDir := filepath.Join(tmp, "downloads")

	releaseDir := filepath.Join(downloadDir, "Some.Release-GRP")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseDir, "readme.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	year := 2023
	cfg := OrganizeConfig{Enabled: true, MoviesDir: moviesDir, OutputDir: downloadDir}
	result := &Result{FilePath: releaseDir, FileName: filepath.Base(releaseDir), Method: MethodTorrent}
	task := &Task{ContentType: "movie", ContentTitle: "Some Release", ContentYear: &year, Title: "Some.Release-GRP"}

	dest, err := organize(result, task, cfg)
	if err != nil {
		t.Fatalf("organize failed: %v", err)
	}
	// Fallback keeps the dir (as a directory) somewhere under MoviesDir.
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("fallback destination not found: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("no-video fallback should keep a directory, got a file: %s", dest)
	}
}

// TestOrganizeDropsRedundantIdenticalDownload (RC-8 cause-fix) asserts that when a
// re-download is BYTE-IDENTICAL to the file already in the library, organize drops
// the source instead of cloning it into a "(2)" sibling (the duplicate-flood cause).
// A DIFFERENT-content download of the same title still coexists as a version sibling.
func TestOrganizeDropsRedundantIdenticalDownload(t *testing.T) {
	tmp := t.TempDir()
	moviesDir := filepath.Join(tmp, "Movies")
	downloadDir := filepath.Join(tmp, "downloads")
	year := 2023
	cfg := OrganizeConfig{Enabled: true, MoviesDir: moviesDir, OutputDir: downloadDir}

	content := make([]byte, 2*1024*1024)
	for i := range content[:4096] {
		content[i] = 'X'
	}

	writeIdentical := func(sub string) string {
		p := filepath.Join(downloadDir, sub, "Movie.2023.1080p.mkv")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// First download → lands canonically.
	src1 := writeIdentical("d1")
	r1 := &Result{FilePath: src1, FileName: "Movie.2023.1080p.mkv", Method: MethodTorrent}
	task := &Task{ContentType: "movie", ContentTitle: "Movie", ContentYear: &year, Title: "Movie 2023 1080p"}
	dest1, err := organize(r1, task, cfg)
	if err != nil {
		t.Fatalf("first organize: %v", err)
	}

	// Second download of the SAME bytes → must NOT create a sibling; source dropped,
	// existing path returned.
	src2 := writeIdentical("d2")
	r2 := &Result{FilePath: src2, FileName: "Movie.2023.1080p.mkv", Method: MethodTorrent}
	dest2, err := organize(r2, task, cfg)
	if err != nil {
		t.Fatalf("second organize: %v", err)
	}
	if dest2 != dest1 {
		t.Errorf("identical re-download should return the existing path %q, got %q (cloned a sibling)", dest1, dest2)
	}
	if _, err := os.Stat(src2); !os.IsNotExist(err) {
		t.Errorf("redundant source should have been removed")
	}
	// Exactly ONE file in the movie dir.
	entries, _ := os.ReadDir(filepath.Dir(dest1))
	var videos int
	for _, e := range entries {
		if isVideoFile(e.Name()) {
			videos++
		}
	}
	if videos != 1 {
		t.Errorf("expected exactly 1 video after identical re-download, got %d", videos)
	}

	// A DIFFERENT-content download of the same title still coexists.
	diff := make([]byte, 2*1024*1024)
	for i := range diff[:4096] {
		diff[i] = 'Y'
	}
	src3 := filepath.Join(downloadDir, "d3", "Movie.2023.2160p.mkv")
	if err := os.MkdirAll(filepath.Dir(src3), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src3, diff, 0o644); err != nil {
		t.Fatal(err)
	}
	r3 := &Result{FilePath: src3, FileName: "Movie.2023.1080p.mkv", Method: MethodTorrent}
	dest3, err := organize(r3, task, cfg)
	if err != nil {
		t.Fatalf("third organize: %v", err)
	}
	if dest3 == dest1 {
		t.Errorf("distinct-content download must NOT collapse onto the existing file")
	}
	if _, err := os.Stat(dest3); err != nil {
		t.Errorf("distinct version should exist: %v", err)
	}
}
