package library

import (
	"context"
	"errors"
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

// A scan whose context is cancelled must (a) stop spawning probes and (b) report
// an error instead of returning a partial cache as if it were complete.
//
// Both halves are the 2026-07-21 incident. The loop used `break` inside a
// `select`, which only exits the SELECT — so after cancellation it kept
// launching a probe per remaining file, each failing instantly with
// "context canceled", and BuildSyncItems synced every one as damaged/
// "unreadable". And returning (cache, nil) let runAutoScan claim fullCycle on a
// truncated scan, whose stale-cleanup DELETEs every row the scan never reached.
func TestScanCancelledContextFailsInsteadOfFlaggingFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.mkv", "b.mkv", "c.mkv", "d.mkv", "e.mkv"} {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		// Sparse file over the 100MB discovery floor — no real bytes written.
		if err := f.Truncate(minFileSize + 1); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead before the first iteration

	cache, err := Scan(ctx, dir, nil, ScanOptions{Workers: 2})
	if err == nil {
		t.Fatalf("cancelled scan returned nil error (caller would claim fullCycle); cache=%+v", cache)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error should wrap context.Canceled, got %v", err)
	}
	if cache != nil {
		t.Errorf("cancelled scan must not return a cache, got %d items", len(cache.Items))
	}
}
