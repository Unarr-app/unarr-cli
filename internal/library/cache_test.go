package library

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveCacheAndLoad(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "library.json")

	cache := &LibraryCache{
		Version:   cacheVersion,
		ScannedAt: "2026-03-29T10:00:00Z",
		Path:      "/media/movies",
		Items: []LibraryItem{
			{
				FilePath: "/media/movies/Inception.mkv",
				FileName: "Inception.mkv",
				FileSize: 5000000000,
				ModTime:  "2026-01-15T12:00:00Z",
				Title:    "Inception",
				Year:     "2010",
				Quality:  "1080p",
			},
		},
	}

	// Save
	if err := SaveCacheTo(cache, path); err != nil {
		t.Fatalf("SaveCacheTo: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache file not found: %v", err)
	}

	// Load
	loaded, err := LoadCacheFrom(path)
	if err != nil {
		t.Fatalf("LoadCacheFrom: %v", err)
	}
	if loaded == nil {
		t.Fatal("loaded cache is nil")
	}

	if loaded.Version != cacheVersion {
		t.Errorf("version = %d, want %d", loaded.Version, cacheVersion)
	}
	if loaded.Path != "/media/movies" {
		t.Errorf("path = %q, want %q", loaded.Path, "/media/movies")
	}
	if len(loaded.Items) != 1 {
		t.Fatalf("items count = %d, want 1", len(loaded.Items))
	}
	if loaded.Items[0].Title != "Inception" {
		t.Errorf("title = %q, want %q", loaded.Items[0].Title, "Inception")
	}
}

func TestLoadCacheNonExistent(t *testing.T) {
	cache, err := LoadCacheFrom("/tmp/nonexistent-unarr-test.json")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if cache != nil {
		t.Fatalf("expected nil cache, got: %v", cache)
	}
}

func TestBuildCacheIndex(t *testing.T) {
	cache := &LibraryCache{
		Items: []LibraryItem{
			{FilePath: "/a.mkv"},
			{FilePath: "/b.mkv"},
			{FilePath: "/c.mkv"},
		},
	}

	idx := BuildCacheIndex(cache)
	if idx["/a.mkv"] != 0 {
		t.Errorf("expected index 0 for /a.mkv, got %d", idx["/a.mkv"])
	}
	if idx["/b.mkv"] != 1 {
		t.Errorf("expected index 1 for /b.mkv, got %d", idx["/b.mkv"])
	}
	if idx["/c.mkv"] != 2 {
		t.Errorf("expected index 2 for /c.mkv, got %d", idx["/c.mkv"])
	}
}

func TestBuildCacheIndexNil(t *testing.T) {
	idx := BuildCacheIndex(nil)
	if idx != nil {
		t.Errorf("expected nil, got %v", idx)
	}
}
