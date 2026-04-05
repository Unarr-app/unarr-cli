package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceFile(t *testing.T) {
	tmp := t.TempDir()
	backupDir := filepath.Join(tmp, "backups")

	// Create "old" file
	oldPath := filepath.Join(tmp, "movie.mkv")
	os.WriteFile(oldPath, []byte("old content"), 0o644)

	// Create "new" file
	newPath := filepath.Join(tmp, "movie-new.mkv")
	os.WriteFile(newPath, []byte("new better content"), 0o644)

	err := replaceFile(oldPath, newPath, backupDir)
	if err != nil {
		t.Fatalf("replaceFile: %v", err)
	}

	// Old path should now contain new content
	data, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatalf("read old path: %v", err)
	}
	if string(data) != "new better content" {
		t.Errorf("old path content = %q, want 'new better content'", string(data))
	}

	// Backup should exist
	entries, _ := os.ReadDir(backupDir)
	if len(entries) != 1 {
		t.Errorf("expected 1 backup file, got %d", len(entries))
	}

	// New file should be gone
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Error("new file should have been moved/deleted")
	}
}

func TestReplaceFileOldNotFound(t *testing.T) {
	tmp := t.TempDir()
	err := replaceFile(filepath.Join(tmp, "nonexistent.mkv"), filepath.Join(tmp, "new.mkv"), "")
	if err == nil {
		t.Error("expected error when old file doesn't exist")
	}
}

func TestCopyFile(t *testing.T) {
	tmp := t.TempDir()

	src := filepath.Join(tmp, "source.txt")
	dst := filepath.Join(tmp, "dest.txt")

	content := []byte("hello world copy test")
	os.WriteFile(src, content, 0o644)

	err := copyFile(src, dst)
	if err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("dest content = %q, want %q", string(data), string(content))
	}
}

func TestCopyFileSrcNotFound(t *testing.T) {
	tmp := t.TempDir()
	err := copyFile(filepath.Join(tmp, "nope.txt"), filepath.Join(tmp, "out.txt"))
	if err == nil {
		t.Error("expected error when source doesn't exist")
	}
}

func TestOrganizeNoDirs(t *testing.T) {
	r := &Result{FilePath: "/tmp/file.mkv", FileName: "file.mkv"}
	task := &Task{Title: "Movie"}

	path, err := organize(r, task, OrganizeConfig{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/tmp/file.mkv" {
		t.Errorf("should return original path when no dirs configured, got %q", path)
	}
}

func TestOrganizeNilResult(t *testing.T) {
	task := &Task{Title: "Movie"}
	path, err := organize(&Result{}, task, OrganizeConfig{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Errorf("expected empty path for empty result, got %q", path)
	}
}

func TestOrganizeMovieDirectory(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src", "MovieDir")
	os.MkdirAll(srcDir, 0o755)
	os.WriteFile(filepath.Join(srcDir, "movie.mkv"), []byte("data"), 0o644)

	moviesDir := filepath.Join(tmp, "Movies")

	r := &Result{FilePath: srcDir, FileName: "MovieDir"}
	task := &Task{Title: "My Movie 2023"}

	path, err := organize(r, task, OrganizeConfig{
		Enabled:   true,
		MoviesDir: moviesDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	if path == srcDir {
		t.Error("directory should have moved")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("organized directory should exist at %s", path)
	}
}

func TestOrganizeSeasonOnly(t *testing.T) {
	tmp := t.TempDir()
	srcFile := filepath.Join(tmp, "Show.S01.Complete.mkv")
	os.WriteFile(srcFile, []byte("data"), 0o644)

	tvDir := filepath.Join(tmp, "TV")

	r := &Result{FilePath: srcFile, FileName: "Show.S01.Complete.mkv"}
	task := &Task{Title: "Show S01"}

	path, err := organize(r, task, OrganizeConfig{
		Enabled:    true,
		TVShowsDir: tvDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Dir(path)
	if filepath.Base(dir) != "Season 01" {
		t.Errorf("expected Season 01 directory, got %q", filepath.Base(dir))
	}
}

// --- Tests for server metadata organize path ---

func intPtr(v int) *int { return &v }

func TestOrganizeShowWithMetadata(t *testing.T) {
	tmp := t.TempDir()
	srcFile := filepath.Join(tmp, "Frieren.Beyond.Journeys.End.S01E03.1080p.WEB-DL.mkv")
	os.WriteFile(srcFile, []byte("data"), 0o644)

	tvDir := filepath.Join(tmp, "TV Shows")

	r := &Result{FilePath: srcFile, FileName: "Frieren.Beyond.Journeys.End.S01E03.1080p.WEB-DL.mkv"}
	task := &Task{
		Title:        "Frieren.Beyond.Journeys.End.S01E03.1080p.WEB-DL",
		ContentType:  "show",
		ContentTitle: "Frieren: Beyond Journey's End",
		Season:       intPtr(1),
		Episode:      intPtr(3),
	}

	path, err := organize(r, task, OrganizeConfig{
		Enabled:    true,
		TVShowsDir: tvDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should be: TV Shows/Frieren - Beyond Journey's End/Season 01/Frieren - Beyond Journey's End - S01E03.mkv
	dir := filepath.Dir(path)
	if filepath.Base(dir) != "Season 01" {
		t.Errorf("expected Season 01 directory, got %q", filepath.Base(dir))
	}
	showDir := filepath.Dir(dir)
	if filepath.Base(showDir) != "Frieren - Beyond Journey's End" {
		t.Errorf("expected show dir 'Frieren - Beyond Journey's End', got %q", filepath.Base(showDir))
	}
	// Filename should be clean
	base := filepath.Base(path)
	if base != "Frieren - Beyond Journey's End - S01E03.mkv" {
		t.Errorf("filename = %q, want 'Frieren - Beyond Journey's End - S01E03.mkv'", base)
	}
}

func TestOrganizeCollectionMovieWithMetadata(t *testing.T) {
	tmp := t.TempDir()
	srcFile := filepath.Join(tmp, "Knives.Out.2019.1080p.BluRay.mkv")
	os.WriteFile(srcFile, []byte("data"), 0o644)

	moviesDir := filepath.Join(tmp, "Movies")

	r := &Result{FilePath: srcFile, FileName: "Knives.Out.2019.1080p.BluRay.mkv"}
	task := &Task{
		Title:          "Knives.Out.2019.1080p.BluRay",
		ContentType:    "movie",
		ContentTitle:   "Knives Out",
		CollectionName: "Knives Out Collection",
	}

	path, err := organize(r, task, OrganizeConfig{
		Enabled:   true,
		MoviesDir: moviesDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should be: Movies/Knives Out Collection/Knives Out (2019)/Knives Out (2019).mkv
	movieDir := filepath.Dir(path)
	if filepath.Base(movieDir) != "Knives Out (2019)" {
		t.Errorf("expected movie dir 'Knives Out (2019)', got %q", filepath.Base(movieDir))
	}
	collDir := filepath.Dir(movieDir)
	if filepath.Base(collDir) != "Knives Out Collection" {
		t.Errorf("expected collection dir 'Knives Out Collection', got %q", filepath.Base(collDir))
	}
	base := filepath.Base(path)
	if base != "Knives Out (2019).mkv" {
		t.Errorf("filename = %q, want 'Knives Out (2019).mkv'", base)
	}
}

func TestOrganizeMovieWithMetadata(t *testing.T) {
	tmp := t.TempDir()
	srcFile := filepath.Join(tmp, "Oppenheimer.2023.2160p.UHD.BluRay.mkv")
	os.WriteFile(srcFile, []byte("data"), 0o644)

	moviesDir := filepath.Join(tmp, "Movies")

	r := &Result{FilePath: srcFile, FileName: "Oppenheimer.2023.2160p.UHD.BluRay.mkv"}
	task := &Task{
		Title:        "Oppenheimer.2023.2160p.UHD.BluRay",
		ContentType:  "movie",
		ContentTitle: "Oppenheimer",
	}

	path, err := organize(r, task, OrganizeConfig{
		Enabled:   true,
		MoviesDir: moviesDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should be: Movies/Oppenheimer (2023)/Oppenheimer (2023).mkv
	movieDir := filepath.Dir(path)
	if filepath.Base(movieDir) != "Oppenheimer (2023)" {
		t.Errorf("expected movie dir 'Oppenheimer (2023)', got %q", filepath.Base(movieDir))
	}
	base := filepath.Base(path)
	if base != "Oppenheimer (2023).mkv" {
		t.Errorf("filename = %q, want 'Oppenheimer (2023).mkv'", base)
	}
}

func TestOrganizeMultipleEpisodesSameFolder(t *testing.T) {
	tmp := t.TempDir()
	tvDir := filepath.Join(tmp, "TV Shows")

	// Simulate two episodes of the same show
	for _, ep := range []int{1, 2} {
		srcFile := filepath.Join(tmp, filepath.Base(t.TempDir())+".mkv")
		os.WriteFile(srcFile, []byte("data"), 0o644)

		r := &Result{FilePath: srcFile, FileName: filepath.Base(srcFile)}
		task := &Task{
			Title:        "Frieren.S01E0" + string(rune('0'+ep)) + ".1080p",
			ContentType:  "show",
			ContentTitle: "Frieren",
			Season:       intPtr(1),
			Episode:      intPtr(ep),
		}

		_, err := organize(r, task, OrganizeConfig{
			Enabled:    true,
			TVShowsDir: tvDir,
		})
		if err != nil {
			t.Fatalf("episode %d: %v", ep, err)
		}
	}

	// Both episodes should be in the same directory
	seasonDir := filepath.Join(tvDir, "Frieren", "Season 01")
	entries, err := os.ReadDir(seasonDir)
	if err != nil {
		t.Fatalf("read season dir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 files in Season 01, got %d", len(entries))
	}
}

func TestOrganizeCleanupSourceDir(t *testing.T) {
	tmp := t.TempDir()
	// Simulate: outputDir/TorrentName/video.mkv + junk files
	outputDir := filepath.Join(tmp, "downloads")
	torrentDir := filepath.Join(outputDir, "Frieren.S01E03.1080p.WEB-DL")
	os.MkdirAll(torrentDir, 0o755)

	srcFile := filepath.Join(torrentDir, "Frieren.S01E03.1080p.WEB-DL.mkv")
	os.WriteFile(srcFile, []byte("video"), 0o644)
	os.WriteFile(filepath.Join(torrentDir, "info.nfo"), []byte("nfo"), 0o644)
	os.WriteFile(filepath.Join(torrentDir, "readme.txt"), []byte("txt"), 0o644)
	os.WriteFile(filepath.Join(torrentDir, "website.url"), []byte("url"), 0o644)

	tvDir := filepath.Join(tmp, "TV Shows")

	r := &Result{FilePath: srcFile, FileName: "Frieren.S01E03.1080p.WEB-DL.mkv"}
	task := &Task{
		Title:        "Frieren.S01E03.1080p.WEB-DL",
		ContentType:  "show",
		ContentTitle: "Frieren",
		Season:       intPtr(1),
		Episode:      intPtr(3),
	}

	path, err := organize(r, task, OrganizeConfig{
		Enabled:    true,
		TVShowsDir: tvDir,
		OutputDir:  outputDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Video should be in organized location
	if _, err := os.Stat(path); err != nil {
		t.Errorf("organized file should exist at %s", path)
	}

	// Source torrent directory should be gone (only had junk left)
	if _, err := os.Stat(torrentDir); !os.IsNotExist(err) {
		t.Errorf("torrent dir should have been cleaned up: %s", torrentDir)
	}

	// OutputDir itself should still exist
	if _, err := os.Stat(outputDir); err != nil {
		t.Errorf("outputDir should still exist")
	}
}

func TestOrganizeNoCleanupWhenVideoRemains(t *testing.T) {
	tmp := t.TempDir()
	outputDir := filepath.Join(tmp, "downloads")
	torrentDir := filepath.Join(outputDir, "MultiVideoTorrent")
	os.MkdirAll(torrentDir, 0o755)

	srcFile := filepath.Join(torrentDir, "episode1.mkv")
	os.WriteFile(srcFile, []byte("video1"), 0o644)
	// Another video file remains
	os.WriteFile(filepath.Join(torrentDir, "episode2.mkv"), []byte("video2"), 0o644)

	tvDir := filepath.Join(tmp, "TV Shows")

	r := &Result{FilePath: srcFile, FileName: "episode1.mkv"}
	task := &Task{
		Title:        "Show S01E01",
		ContentType:  "show",
		ContentTitle: "Show",
		Season:       intPtr(1),
		Episode:      intPtr(1),
	}

	_, err := organize(r, task, OrganizeConfig{
		Enabled:    true,
		TVShowsDir: tvDir,
		OutputDir:  outputDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Torrent dir should still exist because episode2.mkv is still there
	if _, err := os.Stat(torrentDir); err != nil {
		t.Errorf("torrent dir should NOT be cleaned up when video files remain")
	}
}

func TestSanitizePath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Normal Title", "Normal Title"},
		{"Title: Subtitle", "Title - Subtitle"},
		{"Title/Subtitle", "Title-Subtitle"},
		{"What?", "What"},
		{"A*B<C>D|E", "ABCD-E"},
		{"  Spaces  ", "Spaces"},
		{"Trailing...", "Trailing"},
		{"", "Unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizePath(tt.input)
			if got != tt.want {
				t.Errorf("sanitizePath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolveYear(t *testing.T) {
	tests := []struct {
		name string
		task *Task
		want string
	}{
		{"from ContentYear", &Task{ContentYear: intPtr(2023), Title: "Movie.2020.1080p"}, "2023"},
		{"fallback to regex", &Task{Title: "Movie.2020.1080p"}, "2020"},
		{"no year", &Task{Title: "Movie.1080p"}, ""},
		{"zero year fallback", &Task{ContentYear: intPtr(0), Title: "Movie.2019.mkv"}, "2019"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveYear(tt.task)
			if got != tt.want {
				t.Errorf("resolveYear() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsSubtitleFile(t *testing.T) {
	for _, ext := range []string{".srt", ".sub", ".ass", ".ssa", ".vtt", ".idx"} {
		if !isSubtitleFile("file" + ext) {
			t.Errorf("expected %s to be subtitle", ext)
		}
	}
	for _, ext := range []string{".mkv", ".txt", ".nfo", ".jpg"} {
		if isSubtitleFile("file" + ext) {
			t.Errorf("expected %s to NOT be subtitle", ext)
		}
	}
}

func TestMoveSubtitles(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "torrent")
	destDir := filepath.Join(tmp, "dest")
	os.MkdirAll(srcDir, 0o755)
	os.MkdirAll(destDir, 0o755)

	// Create video + subtitles in source
	videoPath := filepath.Join(srcDir, "Movie.2023.1080p.mkv")
	os.WriteFile(videoPath, []byte("video"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "Movie.2023.1080p.srt"), []byte("srt"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "Movie.2023.1080p.en.srt"), []byte("en srt"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "Other.srt"), []byte("other"), 0o644) // should NOT move

	moveSubtitles(videoPath, destDir, "Oppenheimer (2023).mkv")

	// Renamed subtitles should be in dest
	if _, err := os.Stat(filepath.Join(destDir, "Oppenheimer (2023).srt")); err != nil {
		t.Error("expected Oppenheimer (2023).srt in dest")
	}
	if _, err := os.Stat(filepath.Join(destDir, "Oppenheimer (2023).en.srt")); err != nil {
		t.Error("expected Oppenheimer (2023).en.srt in dest")
	}
	// Other.srt should NOT have moved
	if _, err := os.Stat(filepath.Join(srcDir, "Other.srt")); err != nil {
		t.Error("Other.srt should remain in source")
	}
}

func TestMoveSubtitlesNoRename(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "torrent")
	destDir := filepath.Join(tmp, "dest")
	os.MkdirAll(srcDir, 0o755)
	os.MkdirAll(destDir, 0o755)

	videoPath := filepath.Join(srcDir, "Movie.mkv")
	os.WriteFile(videoPath, []byte("video"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "Movie.srt"), []byte("srt"), 0o644)

	moveSubtitles(videoPath, destDir, "") // no rename

	if _, err := os.Stat(filepath.Join(destDir, "Movie.srt")); err != nil {
		t.Error("expected Movie.srt in dest (no rename)")
	}
}

func TestOrganizeMovieWithContentYear(t *testing.T) {
	tmp := t.TempDir()
	srcFile := filepath.Join(tmp, "Oppenheimer.UHD.BluRay.mkv")
	os.WriteFile(srcFile, []byte("data"), 0o644)

	moviesDir := filepath.Join(tmp, "Movies")

	r := &Result{FilePath: srcFile, FileName: "Oppenheimer.UHD.BluRay.mkv"}
	task := &Task{
		Title:        "Oppenheimer.UHD.BluRay", // no year in title!
		ContentType:  "movie",
		ContentTitle: "Oppenheimer",
		ContentYear:  intPtr(2023),
	}

	path, err := organize(r, task, OrganizeConfig{
		Enabled:   true,
		MoviesDir: moviesDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should use ContentYear even though title has no year
	movieDir := filepath.Dir(path)
	if filepath.Base(movieDir) != "Oppenheimer (2023)" {
		t.Errorf("expected movie dir 'Oppenheimer (2023)', got %q", filepath.Base(movieDir))
	}
	base := filepath.Base(path)
	if base != "Oppenheimer (2023).mkv" {
		t.Errorf("filename = %q, want 'Oppenheimer (2023).mkv'", base)
	}
}

func TestCleanTitleEdgeCases(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"Simple Title", "Simple Title"},
		{"Title (2023) 1080p BluRay", "Title"},
		{"Title 720p HDTV", "Title"},
		{"Title x264 HEVC", "Title"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := cleanTitle(tt.input)
			if got != tt.want {
				t.Errorf("cleanTitle(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
