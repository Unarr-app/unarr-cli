package postprocess

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsArchiveFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"movie.rar", true},
		{"movie.RAR", true},
		{"movie.part01.rar", true},
		{"movie.r00", true},
		{"movie.r99", true},
		{"movie.s00", true},
		{"movie.001", true},
		{"movie.099", true},
		{"movie.mkv", false},
		{"movie.mp4", false},
		{"movie.par2", false},
		{"movie.nfo", false},
		{"movie.txt", false},
		{"movie.r", false},
		{"movie.abc", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isArchiveFile(tt.name)
			if got != tt.want {
				t.Errorf("isArchiveFile(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestIsCleanupTarget(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"content.par2", true},
		{"content.PAR2", true},
		{"info.nfo", true},
		{"checksum.sfv", true},
		{"content.nzb", true},
		{"content.srr", true},
		{"content.srs", true},
		{"cover.jpg", true},
		{"cover.png", true},
		{"readme.txt", true},
		{"link.url", true},
		{"movie.rar", true},
		{"movie.r00", true},
		{"movie.s01", true},
		{"movie.001", true},
		{"movie.mkv", false},
		{"movie.mp4", false},
		{"movie.avi", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isCleanupTarget(tt.name)
			if got != tt.want {
				t.Errorf("isCleanupTarget(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestIsNumeric(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"", false},
		{"0", true},
		{"123", true},
		{"00", true},
		{"12a", false},
		{"abc", false},
		{" 1", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isNumeric(tt.input)
			if got != tt.want {
				t.Errorf("isNumeric(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestListExtractedFiles(t *testing.T) {
	dir := t.TempDir()

	// Create some files
	os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte("video"), 0o644)
	os.WriteFile(filepath.Join(dir, "subs.srt"), []byte("subs"), 0o644)
	os.WriteFile(filepath.Join(dir, "movie.rar"), []byte("archive"), 0o644)
	os.WriteFile(filepath.Join(dir, "movie.r00"), []byte("archive part"), 0o644)

	archivePath := filepath.Join(dir, "movie.rar")
	files, err := listExtractedFiles(dir, archivePath)
	if err != nil {
		t.Fatalf("listExtractedFiles: %v", err)
	}

	// Should exclude .rar and .r00 (archive files in same dir)
	// Should include movie.mkv and subs.srt
	if len(files) != 2 {
		t.Errorf("expected 2 files, got %d: %v", len(files), files)
	}

	for _, f := range files {
		base := filepath.Base(f)
		if base != "movie.mkv" && base != "subs.srt" {
			t.Errorf("unexpected file: %s", base)
		}
	}
}

func TestCleanup(t *testing.T) {
	dir := t.TempDir()

	// Files that should be removed
	cleanupFiles := []string{"content.par2", "info.nfo", "checksum.sfv", "movie.rar", "movie.r00"}
	for _, name := range cleanupFiles {
		os.WriteFile(filepath.Join(dir, name), []byte("data"), 0o644)
	}

	// Files that should be kept
	keepFiles := []string{"movie.mkv", "subs.srt"}
	for _, name := range keepFiles {
		os.WriteFile(filepath.Join(dir, name), []byte("data"), 0o644)
	}

	err := Cleanup(dir)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// Verify cleanup files are gone
	for _, name := range cleanupFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed", name)
		}
	}

	// Verify kept files still exist
	for _, name := range keepFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s to exist, got: %v", name, err)
		}
	}
}

func TestPasswordError(t *testing.T) {
	err := &PasswordError{Archive: "/tmp/movie.rar"}
	msg := err.Error()
	if msg != "archive is password protected: /tmp/movie.rar" {
		t.Errorf("PasswordError.Error() = %q", msg)
	}
}
