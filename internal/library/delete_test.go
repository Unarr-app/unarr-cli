package library

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/torrentclaw/unarr/internal/agent"
)

// ---------------------------------------------------------------------------
// isWithinScanPaths
// ---------------------------------------------------------------------------

func TestIsWithinScanPaths(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		scanPaths []string
		want      bool
	}{
		{
			name:      "file inside scan path",
			path:      "/media/movies/Inception.mkv",
			scanPaths: []string{"/media/movies"},
			want:      true,
		},
		{
			name:      "file in subdirectory of scan path",
			path:      "/media/movies/2024/Inception/Inception.mkv",
			scanPaths: []string{"/media/movies"},
			want:      true,
		},
		{
			name:      "file at scan path root itself",
			path:      "/media/movies",
			scanPaths: []string{"/media/movies"},
			want:      false, // rel == "."
		},
		{
			name:      "file outside all scan paths",
			path:      "/tmp/evil.mkv",
			scanPaths: []string{"/media/movies", "/media/shows"},
			want:      false,
		},
		{
			name:      "dotdot traversal attempt",
			path:      "/media/movies/../../../etc/passwd",
			scanPaths: []string{"/media/movies"},
			want:      false,
		},
		{
			name:      "multiple scan paths file in second",
			path:      "/media/shows/Breaking.Bad.S01E01.mkv",
			scanPaths: []string{"/media/movies", "/media/shows"},
			want:      true,
		},
		{
			name:      "empty scan paths",
			path:      "/media/movies/file.mkv",
			scanPaths: []string{},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isWithinScanPaths(tt.path, tt.scanPaths)
			if got != tt.want {
				t.Errorf("isWithinScanPaths(%q, %v) = %v, want %v", tt.path, tt.scanPaths, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// dirEligibleForPrune
// ---------------------------------------------------------------------------

func TestDirEligibleForPrune(t *testing.T) {
	tests := []struct {
		name      string
		dir       string
		scanPaths []string
		want      bool
	}{
		{
			name:      "scan root itself is NOT eligible",
			dir:       "/media/movies",
			scanPaths: []string{"/media/movies"},
			want:      false,
		},
		{
			name:      "subdirectory IS eligible",
			dir:       "/media/movies/2024",
			scanPaths: []string{"/media/movies"},
			want:      true,
		},
		{
			name:      "parent of scan path is NOT eligible",
			dir:       "/media",
			scanPaths: []string{"/media/movies"},
			want:      false,
		},
		{
			name:      "trailing slash normalization — root not eligible",
			dir:       "/media/movies",
			scanPaths: []string{"/media/movies/"},
			want:      false, // filepath.Clean removes trailing slash
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dirEligibleForPrune(tt.dir, tt.scanPaths)
			if got != tt.want {
				t.Errorf("dirEligibleForPrune(%q, %v) = %v, want %v", tt.dir, tt.scanPaths, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// deleteOne
// ---------------------------------------------------------------------------

func TestDeleteOne(t *testing.T) {
	t.Run("delete existing file inside scan path", func(t *testing.T) {
		root := t.TempDir()
		file := filepath.Join(root, "movie.mkv")
		if err := os.WriteFile(file, []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}

		if err := deleteOne(file, []string{root}); err != nil {
			t.Fatalf("deleteOne returned error: %v", err)
		}

		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Error("file should have been deleted")
		}
	})

	t.Run("reject relative path", func(t *testing.T) {
		root := t.TempDir()
		err := deleteOne("relative/path.mkv", []string{root})
		if err == nil {
			t.Fatal("expected error for relative path")
		}
		if got := err.Error(); got != `path is not absolute: "relative/path.mkv"` {
			t.Errorf("unexpected error message: %s", got)
		}
	})

	t.Run("reject path outside scan paths", func(t *testing.T) {
		scanRoot := t.TempDir()
		outsideDir := t.TempDir()
		file := filepath.Join(outsideDir, "secret.txt")
		if err := os.WriteFile(file, []byte("secret"), 0644); err != nil {
			t.Fatal(err)
		}

		err := deleteOne(file, []string{scanRoot})
		if err == nil {
			t.Fatal("expected error for path outside scan paths")
		}

		// File must NOT have been deleted.
		if _, statErr := os.Stat(file); statErr != nil {
			t.Error("file outside scan path should NOT have been deleted")
		}
	})

	t.Run("file already deleted is idempotent", func(t *testing.T) {
		root := t.TempDir()
		// Reference a file that does not exist.
		file := filepath.Join(root, "gone.mkv")

		if err := deleteOne(file, []string{root}); err != nil {
			t.Fatalf("expected idempotent success, got error: %v", err)
		}
	})

	t.Run("symlink pointing outside scan path is rejected", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks require elevated privileges on Windows")
		}

		scanRoot := t.TempDir()
		outsideDir := t.TempDir()
		outsideFile := filepath.Join(outsideDir, "real.mkv")
		if err := os.WriteFile(outsideFile, []byte("real"), 0644); err != nil {
			t.Fatal(err)
		}

		link := filepath.Join(scanRoot, "link.mkv")
		if err := os.Symlink(outsideFile, link); err != nil {
			t.Fatal(err)
		}

		err := deleteOne(link, []string{scanRoot})
		if err == nil {
			t.Fatal("expected error: symlink target is outside scan paths")
		}

		// The real file must NOT have been deleted.
		if _, statErr := os.Stat(outsideFile); statErr != nil {
			t.Error("symlink target outside scan path should NOT have been deleted")
		}
	})

	t.Run("symlink pointing inside scan path is allowed", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks require elevated privileges on Windows")
		}

		scanRoot := t.TempDir()
		subdir := filepath.Join(scanRoot, "sub")
		if err := os.Mkdir(subdir, 0755); err != nil {
			t.Fatal(err)
		}
		realFile := filepath.Join(subdir, "real.mkv")
		if err := os.WriteFile(realFile, []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}

		link := filepath.Join(scanRoot, "link.mkv")
		if err := os.Symlink(realFile, link); err != nil {
			t.Fatal(err)
		}

		if err := deleteOne(link, []string{scanRoot}); err != nil {
			t.Fatalf("deleteOne returned error: %v", err)
		}

		// The real file should have been deleted (os.Remove on resolved path).
		if _, statErr := os.Stat(realFile); !os.IsNotExist(statErr) {
			t.Error("resolved target inside scan path should have been deleted")
		}
	})
}

// ---------------------------------------------------------------------------
// pruneEmptyDirs
// ---------------------------------------------------------------------------

func TestPruneEmptyDirs(t *testing.T) {
	t.Run("empty parent dir is removed", func(t *testing.T) {
		root := t.TempDir()
		sub := filepath.Join(root, "show")
		if err := os.Mkdir(sub, 0755); err != nil {
			t.Fatal(err)
		}

		pruneEmptyDirs(sub, []string{root})

		if _, err := os.Stat(sub); !os.IsNotExist(err) {
			t.Error("empty subdirectory should have been removed")
		}
		// Scan root must still exist.
		if _, err := os.Stat(root); err != nil {
			t.Error("scan path root should NOT have been removed")
		}
	})

	t.Run("non-empty parent dir is NOT removed", func(t *testing.T) {
		root := t.TempDir()
		sub := filepath.Join(root, "show")
		if err := os.Mkdir(sub, 0755); err != nil {
			t.Fatal(err)
		}
		// Put a file inside so it's not empty.
		if err := os.WriteFile(filepath.Join(sub, "keep.txt"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}

		pruneEmptyDirs(sub, []string{root})

		if _, err := os.Stat(sub); err != nil {
			t.Error("non-empty directory should NOT have been removed")
		}
	})

	t.Run("stops at scan path root", func(t *testing.T) {
		root := t.TempDir()
		// Create an empty dir that IS the scan root.
		// pruneEmptyDirs should refuse to remove it.
		pruneEmptyDirs(root, []string{root})

		if _, err := os.Stat(root); err != nil {
			t.Error("scan path root should never be removed")
		}
	})

	t.Run("multi-level cleanup", func(t *testing.T) {
		root := t.TempDir()
		deep := filepath.Join(root, "a", "b", "c")
		if err := os.MkdirAll(deep, 0755); err != nil {
			t.Fatal(err)
		}

		pruneEmptyDirs(deep, []string{root})

		// All three levels (a, a/b, a/b/c) should be removed.
		for _, dir := range []string{
			filepath.Join(root, "a", "b", "c"),
			filepath.Join(root, "a", "b"),
			filepath.Join(root, "a"),
		} {
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("directory should have been removed: %s", dir)
			}
		}

		// Scan root must still exist.
		if _, err := os.Stat(root); err != nil {
			t.Error("scan path root should NOT have been removed")
		}
	})
}

// ---------------------------------------------------------------------------
// DeleteFiles (integration)
// ---------------------------------------------------------------------------

func TestDeleteFiles(t *testing.T) {
	t.Run("multiple items some valid some invalid", func(t *testing.T) {
		root := t.TempDir()
		outsideDir := t.TempDir()
		goodFile := filepath.Join(root, "good.mkv")
		if err := os.WriteFile(goodFile, []byte("ok"), 0644); err != nil {
			t.Fatal(err)
		}
		outsideFile := filepath.Join(outsideDir, "outside.mkv")
		if err := os.WriteFile(outsideFile, []byte("nope"), 0644); err != nil {
			t.Fatal(err)
		}

		items := []agent.LibraryDeleteRequest{
			{ItemID: 1, FilePath: goodFile},                        // valid → deleted
			{ItemID: 2, FilePath: "relative/bad.mkv"},              // relative → rejected
			{ItemID: 3, FilePath: outsideFile},                     // outside scan paths → rejected
			{ItemID: 4, FilePath: filepath.Join(root, "gone.mkv")}, // not-exist → idempotent success
		}

		confirmed := DeleteFiles(items, []string{root})

		// Items 1 and 4 should succeed. Item 2 (relative) and 3 (outside) should fail.
		want := map[int]bool{1: true, 4: true}
		got := make(map[int]bool, len(confirmed))
		for _, id := range confirmed {
			got[id] = true
		}
		if len(got) != len(want) {
			t.Fatalf("confirmed = %v, want IDs %v", confirmed, want)
		}
		for id := range want {
			if !got[id] {
				t.Errorf("expected item %d to be confirmed", id)
			}
		}

		// outsideFile must NOT have been deleted.
		if _, err := os.Stat(outsideFile); err != nil {
			t.Error("file outside scan paths should NOT have been deleted")
		}

		// good.mkv should be deleted.
		if _, err := os.Stat(goodFile); !os.IsNotExist(err) {
			t.Error("good.mkv should have been deleted")
		}
	})

	t.Run("empty scan paths returns nil", func(t *testing.T) {
		items := []agent.LibraryDeleteRequest{
			{ItemID: 1, FilePath: "/some/file.mkv"},
		}
		confirmed := DeleteFiles(items, []string{})
		if confirmed != nil {
			t.Errorf("expected nil, got %v", confirmed)
		}
	})

	t.Run("all relative scan paths returns nil", func(t *testing.T) {
		items := []agent.LibraryDeleteRequest{
			{ItemID: 1, FilePath: "/some/file.mkv"},
		}
		confirmed := DeleteFiles(items, []string{"relative/path", "another/relative"})
		if confirmed != nil {
			t.Errorf("expected nil, got %v", confirmed)
		}
	})

	t.Run("mixed absolute and relative scan paths uses only absolute", func(t *testing.T) {
		root := t.TempDir()
		file := filepath.Join(root, "movie.mkv")
		if err := os.WriteFile(file, []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}

		items := []agent.LibraryDeleteRequest{
			{ItemID: 10, FilePath: file},
		}
		confirmed := DeleteFiles(items, []string{"relative/bad", root})

		if len(confirmed) != 1 || confirmed[0] != 10 {
			t.Errorf("confirmed = %v, want [10]", confirmed)
		}
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Error("file should have been deleted via the absolute scan path")
		}
	})
}
