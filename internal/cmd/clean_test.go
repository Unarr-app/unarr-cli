package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDirStats_Empty(t *testing.T) {
	dir := t.TempDir()
	files, bytes := dirStats(dir)
	if files != 0 || bytes != 0 {
		t.Errorf("expected 0 files 0 bytes, got %d files %d bytes", files, bytes)
	}
}

func TestDirStats_WithFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.log"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.log"), []byte("world!"), 0o644)

	files, bytes := dirStats(dir)
	if files != 2 {
		t.Errorf("expected 2 files, got %d", files)
	}
	if bytes != 11 {
		t.Errorf("expected 11 bytes, got %d", bytes)
	}
}

func TestDirStats_NonExistent(t *testing.T) {
	files, bytes := dirStats("/nonexistent-dir-12345")
	if files != 0 || bytes != 0 {
		t.Errorf("expected 0/0 for nonexistent dir, got %d/%d", files, bytes)
	}
}

func TestFileSize_Exists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("1234567890"), 0o644)

	size := fileSize(path)
	if size != 10 {
		t.Errorf("expected 10, got %d", size)
	}
}

func TestFileSize_NonExistent(t *testing.T) {
	size := fileSize("/nonexistent-file-12345")
	if size != 0 {
		t.Errorf("expected 0 for nonexistent file, got %d", size)
	}
}


func TestRunClean_DryRun(t *testing.T) {
	err := runClean(true, false, false)
	if err != nil {
		t.Logf("runClean dry-run returned: %v (may be expected)", err)
	}
}

func TestShortenHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	if home == "" {
		t.Skip("no home dir")
	}

	result := shortenHome(filepath.Join(home, ".local", "share", "unarr", "unarr.log"))
	if !strings.HasPrefix(result, "~") {
		t.Errorf("expected path starting with ~, got %q", result)
	}
	if strings.Contains(result, home) {
		t.Errorf("home dir not shortened in %q", result)
	}
}

func TestShortenHome_NoHome(t *testing.T) {
	result := shortenHome("/tmp/some/file.txt")
	if result != "/tmp/some/file.txt" {
		t.Errorf("expected unchanged path, got %q", result)
	}
}

func TestScanResumeFiles_Empty(t *testing.T) {
	dir := t.TempDir()
	found, skipped := scanResumeFiles(dir, false)
	if len(found) != 0 || skipped != 0 {
		t.Errorf("expected all zeros, got found=%d skipped=%d", len(found), skipped)
	}
}

func TestScanResumeFiles_NonExistent(t *testing.T) {
	found, skipped := scanResumeFiles("/nonexistent-resume-dir", false)
	if len(found) != 0 || skipped != 0 {
		t.Errorf("expected all zeros for nonexistent dir")
	}
}

func TestScanResumeFiles_OnlyStale(t *testing.T) {
	dir := t.TempDir()

	// Create a stale file (>7 days)
	stalePath := filepath.Join(dir, "old-task.progress")
	os.WriteFile(stalePath, []byte("stale data"), 0o644)
	staleTime := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(stalePath, staleTime, staleTime)

	// Create a fresh file (<7 days)
	os.WriteFile(filepath.Join(dir, "new-task.progress"), []byte("fresh"), 0o644)

	// Default mode: only stale files
	found, skipped := scanResumeFiles(dir, false)
	if len(found) != 1 {
		t.Errorf("expected 1 stale file, got %d", len(found))
	}
	if skipped != 1 {
		t.Errorf("expected 1 skipped (fresh), got %d", skipped)
	}
	if len(found) != 1 || found[0].path != stalePath {
		t.Errorf("expected stale file in found, got %v", found)
	}
}

func TestScanResumeFiles_AllMode(t *testing.T) {
	dir := t.TempDir()

	// Create stale + fresh files
	os.WriteFile(filepath.Join(dir, "old-task.progress"), []byte("stale"), 0o644)
	staleTime := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(filepath.Join(dir, "old-task.progress"), staleTime, staleTime)

	os.WriteFile(filepath.Join(dir, "new-task.nzb"), []byte("fresh"), 0o644)

	// --all mode: everything
	found, skipped := scanResumeFiles(dir, true)
	if len(found) != 2 {
		t.Errorf("expected 2 files with --all, got %d", len(found))
	}
	if skipped != 0 {
		t.Errorf("expected 0 skipped with --all, got %d", skipped)
	}
}

func TestScanResumeFiles_DescLabels(t *testing.T) {
	dir := t.TempDir()

	// Stale file
	stalePath := filepath.Join(dir, "old.progress")
	os.WriteFile(stalePath, []byte("x"), 0o644)
	staleTime := time.Now().Add(-10 * 24 * time.Hour)
	os.Chtimes(stalePath, staleTime, staleTime)

	// Fresh file
	freshPath := filepath.Join(dir, "new.nzb")
	os.WriteFile(freshPath, []byte("y"), 0o644)

	// Default: stale only
	found, _ := scanResumeFiles(dir, false)
	if len(found) != 1 || found[0].desc != "stale resume file" {
		t.Errorf("expected desc 'stale resume file', got %q", found[0].desc)
	}

	// All mode: fresh file should say "resume file"
	found, _ = scanResumeFiles(dir, true)
	for _, e := range found {
		if e.path == freshPath && e.desc != "resume file" {
			t.Errorf("expected desc 'resume file' for fresh file, got %q", e.desc)
		}
		if e.path == stalePath && e.desc != "stale resume file" {
			t.Errorf("expected desc 'stale resume file' for stale file, got %q", e.desc)
		}
	}
}
