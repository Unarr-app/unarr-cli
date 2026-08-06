package postprocess

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFiles creates empty files with the given names inside dir.
func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestFindFirstRarInDir_PrefersExplicitPart01(t *testing.T) {
	dir := t.TempDir()
	// Deliberately out of order, and with a SHORTER middle-part name than the
	// part01: priority 1 must win on semantics, not on name length.
	writeFiles(t, dir, "show.part9.rar", "show.part02.rar", "show.part01.rar")

	got := findFirstRarInDir(dir)
	if filepath.Base(got) != "show.part01.rar" {
		t.Errorf("want show.part01.rar, got %q", filepath.Base(got))
	}
}

func TestFindFirstRarInDir_ShortestPlainRar(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "movie.rar", "movie.r00", "movie.r01", "movie.sample.rar")

	got := findFirstRarInDir(dir)
	if filepath.Base(got) != "movie.rar" {
		t.Errorf("want movie.rar, got %q", filepath.Base(got))
	}
}

// A part-numbered set whose entry volume is missing must NOT be handed to the
// extractor: unrar fails on a middle volume, so "left packed" beats "extracted
// into an error".
func TestFindFirstRarInDir_SkipsPartSetWithoutFirstVolume(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "show.part04.rar", "show.part05.rar")

	if got := findFirstRarInDir(dir); got != "" {
		t.Errorf("want no archive (entry volume missing), got %q", filepath.Base(got))
	}
}

func TestFindFirstRarInDir_SplitFormat(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "release.002", "release.001", "release.003")

	got := findFirstRarInDir(dir)
	if filepath.Base(got) != "release.001" {
		t.Errorf("want release.001, got %q", filepath.Base(got))
	}
}

func TestFindFirstRarInDir_IgnoresSubdirectories(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "Extras")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, sub, "extras.rar")

	if got := findFirstRarInDir(dir); got != "" {
		t.Errorf("want no archive at top level, got %q", got)
	}
}

// THE no-op guarantee: callers invoke ExtractInDir unconditionally, so a plain
// release must sail through untouched and report nothing.
func TestExtractInDir_NoArchiveIsNoOp(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "Movie.2026.1080p.mkv", "Movie.2026.1080p.srt")

	res, err := ExtractInDir(dir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Extracted {
		t.Error("Extracted = true for a release with no archive")
	}
	if res.Note != "" {
		t.Errorf("want empty Note, got %q", res.Note)
	}
	// Nothing may be removed.
	if _, err := os.Stat(filepath.Join(dir, "Movie.2026.1080p.mkv")); err != nil {
		t.Errorf("video file disappeared: %v", err)
	}
}

func TestExtractInDir_UnreadableDirIsNoOp(t *testing.T) {
	res, err := ExtractInDir(filepath.Join(t.TempDir(), "does-not-exist"), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Extracted {
		t.Error("Extracted = true for a missing directory")
	}
}

// CONTRAFACTUAL: proves the bug this fix exists for is real. A packed release
// contains NO video, which is precisely the condition that made organizeDir
// fall back to moving the raw folder — the user-visible pile of .rNN files.
//
// If this ever fails, the premise of the fix is wrong and the fix must be
// re-examined rather than the test relaxed.
func TestPackedReleaseHasNoVideo_TheBugPremise(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir,
		"show.s01e01.part01.rar",
		"show.s01e01.part02.rar",
		"show.s01e01.nfo",
	)

	videoExts := map[string]bool{".mkv": true, ".mp4": true, ".avi": true}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if videoExts[filepath.Ext(e.Name())] {
			t.Fatalf("premise broken: packed release already exposes a video (%s)", e.Name())
		}
	}

	// And the archive IS detected — so the fix has something to act on.
	if got := findFirstRarInDir(dir); got == "" {
		t.Fatal("packed release: no archive detected, fix would never trigger")
	}
}

// A missing extractor must degrade to "left packed", never to an error: the raw
// parts are still what the swarm served.
func TestExtractInDir_NoExtractorLeavesReleaseIntact(t *testing.T) {
	if _, path := FindExtractor(); path != "" {
		t.Skip("an extractor is installed; cannot exercise the missing-binary path")
	}

	dir := t.TempDir()
	writeFiles(t, dir, "movie.part01.rar", "movie.part02.rar")

	res, err := ExtractInDir(dir, "")
	if err != nil {
		t.Fatalf("missing extractor must not be an error, got %v", err)
	}
	if res.Extracted {
		t.Error("Extracted = true without an extractor")
	}
	if res.Note == "" {
		t.Error("want a Note explaining the release was left packed")
	}
	if _, err := os.Stat(filepath.Join(dir, "movie.part01.rar")); err != nil {
		t.Errorf("archive part was removed despite no extraction: %v", err)
	}
}
