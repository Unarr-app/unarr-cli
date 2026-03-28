package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOrganizeDisabled(t *testing.T) {
	r := &Result{FilePath: "/tmp/file.mkv", FileName: "file.mkv"}
	task := &Task{Title: "Movie"}
	path, err := organize(r, task, OrganizeConfig{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/tmp/file.mkv" {
		t.Errorf("path = %q, want original path when disabled", path)
	}
}

func TestOrganizeMovie(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	os.MkdirAll(srcDir, 0o755)
	srcFile := filepath.Join(srcDir, "Movie.2023.1080p.mkv")
	os.WriteFile(srcFile, []byte("data"), 0o644)

	moviesDir := filepath.Join(tmp, "Movies")

	r := &Result{FilePath: srcFile, FileName: "Movie.2023.1080p.mkv"}
	task := &Task{Title: "Movie 2023"}

	path, err := organize(r, task, OrganizeConfig{
		Enabled:   true,
		MoviesDir: moviesDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should be in Movies/Movie (2023)/
	if path == srcFile {
		t.Error("file should have moved")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("organized file should exist at %s: %v", path, err)
	}
}

func TestOrganizeTVShow(t *testing.T) {
	tmp := t.TempDir()
	srcFile := filepath.Join(tmp, "Show.S02E05.1080p.mkv")
	os.WriteFile(srcFile, []byte("data"), 0o644)

	tvDir := filepath.Join(tmp, "TV Shows")

	r := &Result{FilePath: srcFile, FileName: "Show.S02E05.1080p.mkv"}
	task := &Task{Title: "Show S02E05"}

	path, err := organize(r, task, OrganizeConfig{
		Enabled:    true,
		TVShowsDir: tvDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should detect season from filename S02
	if _, err := os.Stat(path); err != nil {
		t.Errorf("organized file should exist at %s: %v", path, err)
	}
}

func TestOrganizeTVShowAltFormat(t *testing.T) {
	tmp := t.TempDir()
	srcFile := filepath.Join(tmp, "Show.3x12.HDTV.mkv")
	os.WriteFile(srcFile, []byte("data"), 0o644)

	tvDir := filepath.Join(tmp, "TV Shows")

	r := &Result{FilePath: srcFile, FileName: "Show.3x12.HDTV.mkv"}
	task := &Task{Title: "Show 3x12"}

	path, err := organize(r, task, OrganizeConfig{
		Enabled:    true,
		TVShowsDir: tvDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should detect season 03 from "3x12" format
	if _, err := os.Stat(path); err != nil {
		t.Errorf("organized file should exist at %s: %v", path, err)
	}
	// Verify it went into Season 03 directory
	dir := filepath.Dir(path)
	if filepath.Base(dir) != "Season 03" {
		t.Errorf("expected Season 03 directory, got %q", filepath.Base(dir))
	}
}

func TestSeasonDetectionFormats(t *testing.T) {
	tests := []struct {
		filename string
		wantTV   bool
	}{
		{"Show.S01E05.720p.mkv", true},
		{"Show.s02e10.1080p.mkv", true},
		{"Show.3x12.HDTV.mkv", true},
		{"Show.12x01.mkv", true},
		{"Movie.2023.1080p.mkv", false},
		{"Just.A.Movie.mkv", false},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			isTV := episodeRegex.MatchString(tt.filename) ||
				altEpRegex.MatchString(tt.filename) ||
				seasonRegex.MatchString(tt.filename)
			if isTV != tt.wantTV {
				t.Errorf("isTV(%q) = %v, want %v", tt.filename, isTV, tt.wantTV)
			}
		})
	}
}

func TestCleanTitle(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"The Matrix (1999)", "The Matrix"},
		{"Oppenheimer 2023 1080p BluRay", "Oppenheimer 2023"},
		{"Movie", "Movie"},
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
