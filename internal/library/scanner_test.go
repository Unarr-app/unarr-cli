package library

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverFiles(t *testing.T) {
	dir := t.TempDir()

	// Create video files (need to be >= 100MB to pass size check)
	largeContent := make([]byte, 101*1024*1024)

	videoFiles := []string{"movie.mkv", "show.mp4", "clip.avi"}
	for _, name := range videoFiles {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, largeContent, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// Non-video files (should be excluded)
	nonVideo := []string{"readme.txt", "cover.jpg", "subs.srt"}
	for _, name := range nonVideo {
		if err := os.WriteFile(filepath.Join(dir, name), largeContent, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// Small video file (should be excluded, < 100MB)
	if err := os.WriteFile(filepath.Join(dir, "small.mkv"), []byte("small"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Excluded pattern (sample)
	sampleDir := filepath.Join(dir, "sample")
	os.MkdirAll(sampleDir, 0o755)
	if err := os.WriteFile(filepath.Join(sampleDir, "sample.mkv"), largeContent, 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := discoverFiles(dir)
	if err != nil {
		t.Fatalf("discoverFiles: %v", err)
	}

	if len(files) != 3 {
		t.Errorf("expected 3 files, got %d: %v", len(files), files)
	}

	// Check that all returned files are video extensions
	for _, f := range files {
		ext := filepath.Ext(f)
		if ext != ".mkv" && ext != ".mp4" && ext != ".avi" {
			t.Errorf("unexpected extension: %s", ext)
		}
	}
}

func TestDiscoverFilesEmptyDir(t *testing.T) {
	dir := t.TempDir()

	files, err := discoverFiles(dir)
	if err != nil {
		t.Fatalf("discoverFiles: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files, got %d", len(files))
	}
}

func TestDiscoverFilesExcludePatterns(t *testing.T) {
	dir := t.TempDir()
	largeContent := make([]byte, 101*1024*1024)

	excludeDirs := []string{"trailer", "featurette", "extras", "bonus"}
	for _, name := range excludeDirs {
		sub := filepath.Join(dir, name)
		os.MkdirAll(sub, 0o755)
		if err := os.WriteFile(filepath.Join(sub, "video.mkv"), largeContent, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	files, err := discoverFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files (all excluded), got %d: %v", len(files), files)
	}
}
