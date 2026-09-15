package postprocess

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestIsRolledOverVolume(t *testing.T) {
	names := map[string]bool{
		"movie.rar": true, "movie.r99": true, "movie.s99": true, "movie.t00": true, "movie.t05": true,
		"game.rar": true, "game.z64": true, "tape.t64": true,
	}
	tests := []struct {
		name string
		want bool
	}{
		{"movie.t00", true},
		{"Movie.T05", true},
		{"movie.u00", false}, // no movie.t99 beside it
		{"game.z64", false},  // extracted ROM next to its archive
		{"tape.t64", false},
		{"movie.s99", false}, // classic volume: isCleanupTarget's job
		{"movie.mkv", false},
	}
	for _, tt := range tests {
		if got := isRolledOverVolume(tt.name, names); got != tt.want {
			t.Errorf("isRolledOverVolume(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestListExtractedFilesSkipsRolledOverVolumes: .tNN volumes of the unpacked set
// are not reported as extracted output, but a ROM extracted beside them is.
func TestListExtractedFilesSkipsRolledOverVolumes(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"movie.rar", "movie.r99", "movie.s99", "movie.t00", "movie.t01", "game.z64"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := listExtractedFiles(dir, filepath.Join(dir, "movie.rar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || filepath.Base(got[0]) != "game.z64" {
		t.Fatalf("listExtractedFiles = %v, want only game.z64", got)
	}
}

// TestArchiveVolumesOfIncludesRolledOverVolumes: a set that rolled over past
// .s99 loses its .tNN volumes on cleanup too, while a ROM extracted beside it and
// another set's volumes stay.
func TestArchiveVolumesOfIncludesRolledOverVolumes(t *testing.T) {
	dir := t.TempDir()
	files := []string{"movie.rar", "movie.r99", "movie.s99", "movie.t00", "movie.t01", "movie.z64", "other.rar", "other.t00"}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, p := range archiveVolumesOf(dir, filepath.Join(dir, "movie.rar")) {
		got = append(got, filepath.Base(p))
	}
	sort.Strings(got)
	want := []string{"movie.r99", "movie.rar", "movie.s99", "movie.t00", "movie.t01"}
	if len(got) != len(want) {
		t.Fatalf("archiveVolumesOf = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("archiveVolumesOf = %v, want %v", got, want)
		}
	}
}
