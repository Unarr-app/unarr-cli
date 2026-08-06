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
	writeFiles(t, dir, "release.002", "release.003")
	// The entry volume must carry a real archive header: a .00N name alone is
	// not enough (see TestFindFirstRarInDir_RejectsSplitDataWithoutArchiveMagic
	// — raw data chunks share that convention and must not be "extracted").
	if err := os.WriteFile(filepath.Join(dir, "release.001"), []byte("PK\x03\x04zip payload"), 0o644); err != nil {
		t.Fatal(err)
	}

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
//
// Stubs the lookup instead of skipping when an extractor is installed — the
// earlier version skipped on any dev machine and in CI, so it never actually
// ran and would have passed with this branch broken.
func TestExtractInDir_NoExtractorLeavesReleaseIntact(t *testing.T) {
	orig := findExtractorFn
	findExtractorFn = func() (ExtractorType, string) { return ExtractorNone, "" }
	t.Cleanup(func() { findExtractorFn = orig })

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
	for _, name := range []string{"movie.part01.rar", "movie.part02.rar"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("archive part %s was removed despite no extraction: %v", name, err)
		}
	}
}

// REGRESSION (review finding #2): CleanupArchives must remove ONLY the volumes
// of the set that was unpacked. The previous implementation delegated to
// Cleanup(), which sweeps by extension (.txt .jpg .png .nfo …) — safe on a
// usenet scratch dir, destructive on a torrent dir the swarm served. Measured
// then: of 7 files, only the .mkv survived.
func TestCleanupArchives_KeepsUserFiles(t *testing.T) {
	dir := t.TempDir()
	keep := []string{"Movie.mkv", "Movie.en.txt", "poster.jpg", "fanart.png", "my_notes.txt", "release.nfo"}
	parts := []string{"movie.rar", "movie.r00", "movie.r01"}
	writeFiles(t, dir, append(append([]string{}, keep...), parts...)...)

	res := &ExtractDirResult{
		Extracted: true,
		archiveParts: []string{
			filepath.Join(dir, "movie.rar"),
			filepath.Join(dir, "movie.r00"),
			filepath.Join(dir, "movie.r01"),
		},
	}
	if err := CleanupArchives(res); err != nil {
		t.Fatalf("CleanupArchives: %v", err)
	}

	for _, name := range keep {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("user file %s was deleted: %v", name, err)
		}
	}
	// COUNTERFACTUAL: the parts really are gone, so the test above is not
	// passing merely because cleanup did nothing at all.
	for _, name := range parts {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("archive part %s survived cleanup", name)
		}
	}
}

// Two unrelated sets in one directory: only the unpacked one is removed.
func TestCleanupArchives_OnlyTouchesItsOwnSet(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "showA.part01.rar", "showA.part02.rar", "showB.part01.rar", "showB.part02.rar")

	res := &ExtractDirResult{
		Extracted:    true,
		archiveParts: archiveVolumesOf(dir, filepath.Join(dir, "showA.part01.rar")),
	}
	if err := CleanupArchives(res); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"showB.part01.rar", "showB.part02.rar"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("unrelated set member %s was deleted: %v", name, err)
		}
	}
	for _, name := range []string{"showA.part01.rar", "showA.part02.rar"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("own set member %s survived: %v", name, err)
		}
	}
}

// A result that never extracted must delete nothing.
func TestCleanupArchives_NoOpWhenNotExtracted(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "movie.rar", "movie.r00")

	if err := CleanupArchives(&ExtractDirResult{Extracted: false}); err != nil {
		t.Fatal(err)
	}
	if err := CleanupArchives(nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"movie.rar", "movie.r00"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s deleted despite no extraction: %v", name, err)
		}
	}
}

// REGRESSION (review finding #3): raw data chunks that merely use the .00N
// naming convention are NOT an archive. Treating them as one made 7z
// concatenate them and the caller delete the originals.
func TestFindFirstRarInDir_RejectsSplitDataWithoutArchiveMagic(t *testing.T) {
	dir := t.TempDir()
	// Plain bytes: no archive header anywhere.
	for _, name := range []string{"video.001", "video.002", "video.003"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not an archive at all"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if got := findFirstRarInDir(dir); got != "" {
		t.Errorf("raw split data treated as an archive: %q", filepath.Base(got))
	}
}

// COUNTERFACTUAL for the test above: a .001 that DOES carry a zip header is
// still picked up, so the guard rejects the right thing rather than everything.
func TestFindFirstRarInDir_AcceptsSplitWithArchiveMagic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "release.001"), []byte("PK\x03\x04rest of the zip"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := findFirstRarInDir(dir)
	if filepath.Base(got) != "release.001" {
		t.Errorf("real split archive was rejected, got %q", got)
	}
}

func TestArchiveStem(t *testing.T) {
	cases := map[string]string{
		"show.part01.rar": "show",
		"show.part02.rar": "show",
		"show.rar":        "show",
		"show.r00":        "show",
		"show.zip.001":    "show",
		"show.mkv":        "", // not a volume name
	}
	for name, want := range cases {
		if got := archiveStem(name); got != want {
			t.Errorf("archiveStem(%q) = %q, want %q", name, got, want)
		}
	}
}
