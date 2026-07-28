package library

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
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

		confirmed, failed := DeleteFiles(items, []string{root})

		// A genuine failure on OUR file must be reported (item 2: not absolute).
		// Item 3 lives outside our scan paths → another agent's file → skipped
		// silently, so its owner still gets handed the deletion.
		wantFailed := map[int]bool{2: true}
		gotFailed := make(map[int]bool, len(failed))
		for _, f := range failed {
			gotFailed[f.ID] = true
			if f.Error == "" {
				t.Errorf("item %d failed with an empty reason", f.ID)
			}
		}
		if len(gotFailed) != len(wantFailed) {
			t.Errorf("failed = %v, want IDs %v", failed, wantFailed)
		}
		for id := range wantFailed {
			if !gotFailed[id] {
				t.Errorf("expected item %d to be reported as failed", id)
			}
		}

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

	t.Run("empty scan paths returns nil and reports every item as failed", func(t *testing.T) {
		items := []agent.LibraryDeleteRequest{
			{ItemID: 1, FilePath: "/some/file.mkv"},
		}
		confirmed, failed := DeleteFiles(items, []string{})
		if confirmed != nil {
			t.Errorf("expected nil, got %v", confirmed)
		}
		if len(failed) != 1 || failed[0].ID != 1 || failed[0].Error == "" {
			t.Errorf("a misconfigured agent must report why, got %v", failed)
		}
	})

	t.Run("all relative scan paths returns nil and reports failures", func(t *testing.T) {
		items := []agent.LibraryDeleteRequest{
			{ItemID: 1, FilePath: "/some/file.mkv"},
		}
		confirmed, failed := DeleteFiles(items, []string{"relative/path", "another/relative"})
		if confirmed != nil {
			t.Errorf("expected nil, got %v", confirmed)
		}
		if len(failed) != 1 {
			t.Errorf("expected 1 reported failure, got %v", failed)
		}
	})

	// A file OUTSIDE our scan paths belongs to another agent. We must neither
	// confirm it (that would tombstone a row whose file is alive elsewhere) nor
	// report it failed (that would clear the server's pending flag and the real
	// owner would never be handed the deletion). Stay silent.
	t.Run("file outside scan paths is skipped silently, not confirmed nor failed", func(t *testing.T) {
		root := t.TempDir()
		other := t.TempDir()
		alive := filepath.Join(other, "someone-elses.mkv")
		if err := os.WriteFile(alive, []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
		items := []agent.LibraryDeleteRequest{
			{ItemID: 7, FilePath: alive},
			{ItemID: 8, FilePath: filepath.Join(other, "gone.mkv")}, // missing AND not ours
		}
		confirmed, failed := DeleteFiles(items, []string{root})
		if len(confirmed) != 0 {
			t.Errorf("must NOT confirm another agent's file, got %v", confirmed)
		}
		if len(failed) != 0 {
			t.Errorf("must NOT report another agent's file as failed, got %v", failed)
		}
		if _, err := os.Stat(alive); err != nil {
			t.Error("another agent's file must not be touched")
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
		confirmed, _ := DeleteFiles(items, []string{"relative/bad", root})

		if len(confirmed) != 1 || confirmed[0] != 10 {
			t.Errorf("confirmed = %v, want [10]", confirmed)
		}
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Error("file should have been deleted via the absolute scan path")
		}
	})
}

// TestDeleteOneRemovesSidecars (RC-3) asserts that deleting a video also removes
// its same-basename sidecars (subtitles, .nfo, artwork) so the folder can be
// pruned, while leaving a DIFFERENT video (and thus the dir) untouched.
func TestDeleteOneRemovesSidecars(t *testing.T) {
	root := t.TempDir()
	showDir := filepath.Join(root, "Show", "Season 01")
	if err := os.MkdirAll(showDir, 0o755); err != nil {
		t.Fatal(err)
	}

	video := filepath.Join(showDir, "Show - S01E02.mkv")
	sidecars := []string{
		"Show - S01E02.es.srt",
		"Show - S01E02.vtt",
		"Show - S01E02.nfo",
	}
	otherVideo := filepath.Join(showDir, "Show - S01E03.mkv")

	if err := os.WriteFile(video, []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, s := range sidecars {
		if err := os.WriteFile(filepath.Join(showDir, s), []byte("s"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(otherVideo, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := deleteOne(video, []string{root}); err != nil {
		t.Fatalf("deleteOne returned error: %v", err)
	}

	// Video gone.
	if _, err := os.Stat(video); !os.IsNotExist(err) {
		t.Error("target video should have been deleted")
	}
	// All 3 sidecars gone.
	for _, s := range sidecars {
		if _, err := os.Stat(filepath.Join(showDir, s)); !os.IsNotExist(err) {
			t.Errorf("sidecar %s should have been deleted", s)
		}
	}
	// The OTHER video is untouched.
	if _, err := os.Stat(otherVideo); err != nil {
		t.Errorf("other video should NOT have been deleted: %v", err)
	}
	// The dir is NOT pruned (still holds the other video).
	if _, err := os.Stat(showDir); err != nil {
		t.Errorf("season dir should remain (still has another video): %v", err)
	}
}

// TestSidecarBelongsTo table-drives the boundary rule that stops deleteSidecars /
// moveSubtitles from stealing a different video's sidecar (#2 data-loss fix).
func TestSidecarBelongsTo(t *testing.T) {
	const stem = "Movie"
	tests := []struct {
		name string
		want bool
	}{
		{"Movie.srt", true},              // dotted, bare
		{"Movie.es.srt", true},           // dotted lang chain
		{"Movie.forced.eng.srt", true},   // dotted multi chain
		{"Movie.nfo", true},              // dotted metadata
		{"Movie-poster.jpg", true},       // hyphen artwork
		{"Movie-fanart.jpg", true},       // hyphen artwork
		{"Movie Extended.srt", false},    // SPACE → different title, must NOT belong
		{"Movie Extended.en.srt", false}, // different title with chain
		{"MovieSpecial.srt", false},      // no separator → different title
		{"Movie2.srt", false},            // "Movie2" is a different stem
		{"Other.srt", false},             // unrelated
	}
	for _, tt := range tests {
		if got := SidecarBelongsTo(tt.name, stem); got != tt.want {
			t.Errorf("SidecarBelongsTo(%q, %q) = %v, want %v", tt.name, stem, got, tt.want)
		}
	}
}

// TestDeleteSidecarsBoundary is the end-to-end #2 regression: deleting Movie.mkv
// removes only Movie.srt, and leaves Movie Extended.srt (the subtitle of the still
// present Movie Extended.mkv) untouched.
func TestDeleteSidecarsBoundary(t *testing.T) {
	dir := t.TempDir()
	movie := filepath.Join(dir, "Movie.mkv")
	movieSub := filepath.Join(dir, "Movie.srt")
	otherVideo := filepath.Join(dir, "Movie Extended.mkv")
	otherSub := filepath.Join(dir, "Movie Extended.srt")
	for _, p := range []string{movie, movieSub, otherVideo, otherSub} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// deleteSidecars is called after the video's own removal.
	deleteSidecars(movie)

	if _, err := os.Stat(movieSub); !os.IsNotExist(err) {
		t.Errorf("Movie.srt should have been removed as Movie.mkv's sidecar (err=%v)", err)
	}
	if _, err := os.Stat(otherSub); err != nil {
		t.Errorf("Movie Extended.srt (a DIFFERENT video's subtitle) must survive: %v", err)
	}
	if _, err := os.Stat(otherVideo); err != nil {
		t.Errorf("Movie Extended.mkv must be untouched: %v", err)
	}
}
