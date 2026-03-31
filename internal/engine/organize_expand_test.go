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
