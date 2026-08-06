package postprocess

import (
	"os"
	"path/filepath"
	"strings"
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
//
// REGRESSION (review finding): the earlier version of this test used
// "showA.part01.rar" / "showB.part01.rar" — single-segment names in .partNN
// form, the ONE combination where the stem bug could not show. Real scene names
// are dotted and differ only in a trailing segment, and archiveStem used to drop
// one segment too many, collapsing both sets onto "Movie.2024". Measured then:
// archiveVolumesOf returned 4 files for a 2-file set, and cleanup deleted the
// set that was never unpacked. Table-driven over BOTH volume forms so neither
// can regress unnoticed.
func TestCleanupArchives_OnlyTouchesItsOwnSet(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mine  []string
		other []string
	}{
		{
			name: "plain .rar/.rNN",
			// The DISTINGUISHING segment must be the LAST one before the suffix:
			// that is the only position the old over-trim consumed. With it two
			// segments back (…x264-GRP vs …x264-OTHER) the old code still told the
			// sets apart and this test passed on the broken implementation.
			mine:  []string{"Movie.2024.1080p-GRP.rar", "Movie.2024.1080p-GRP.r00"},
			other: []string{"Movie.2024.720p-OTHER.rar", "Movie.2024.720p-OTHER.r00"},
		},
		{
			name:  "partNN.rar",
			mine:  []string{"Show.S01E01.1080p-GRP.part01.rar", "Show.S01E01.1080p-GRP.part02.rar"},
			other: []string{"Show.S01E01.720p-OTHER.part01.rar", "Show.S01E01.720p-OTHER.part02.rar"},
		},
		{
			name:  "numbered split keeps container stripping",
			mine:  []string{"Movie.2024.1080p-GRP.zip.001", "Movie.2024.1080p-GRP.zip.002"},
			other: []string{"Movie.2024.720p-OTHER.zip.001", "Movie.2024.720p-OTHER.zip.002"},
		},
		{
			// SAME base name, DIFFERENT volume form. The second data-loss round:
			// stripping the container extension made "Movie-GRP.zip.001" and
			// "Movie-GRP.rar" share a key, so unpacking the split deleted the rar
			// set. Measured: 4 files returned for a 2-file set.
			name:  "same base, different form",
			mine:  []string{"Movie.2024-GRP.zip.001", "Movie.2024-GRP.zip.002"},
			other: []string{"Movie.2024-GRP.rar", "Movie.2024-GRP.r00"},
		},
		{
			// The inverse: entering through the rar set must not drag the split's
			// volumes along either.
			name:  "same base, entered from the rar side",
			mine:  []string{"Movie.2024-GRP.rar", "Movie.2024-GRP.r00"},
			other: []string{"Movie.2024-GRP.zip.001", "Movie.2024-GRP.zip.002"},
		},
		{
			// A rar set continues .rar → .r00 → … → .r99 → .s00, so every form
			// must share ONE key. Over-narrowing would leave volumes behind.
			name:  "rar set spanning .rNN and .sNN",
			mine:  []string{"Movie.2024-GRP.rar", "Movie.2024-GRP.r99", "Movie.2024-GRP.s00"},
			other: []string{"Other.2024-GRP.rar", "Other.2024-GRP.r00"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, append(append([]string{}, tc.mine...), tc.other...)...)

			parts := archiveVolumesOf(dir, filepath.Join(dir, tc.mine[0]))
			if len(parts) != len(tc.mine) {
				t.Errorf("archiveVolumesOf returned %d files, want %d — it reached outside its own set",
					len(parts), len(tc.mine))
			}

			res := &ExtractDirResult{Extracted: true, archiveParts: parts}
			if err := CleanupArchives(res); err != nil {
				t.Fatal(err)
			}

			for _, name := range tc.other {
				if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
					t.Errorf("unrelated set member %s was deleted: %v", name, err)
				}
			}
			// COUNTERFACTUAL: our own set really is gone, so the assertions above
			// are not passing merely because cleanup did nothing.
			for _, name := range tc.mine {
				if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
					t.Errorf("own set member %s survived cleanup", name)
				}
			}
		})
	}
}

// archiveStem must not drop a segment of the release name. It decides which
// files get deleted, so a stem shared by two different releases is data loss.
func TestArchiveStem_KeepsFullReleaseName(t *testing.T) {
	cases := map[string]string{
		// Dotted scene names: the whole name before the volume suffix is kept.
		"Movie.2024.1080p.BluRay.x264-GRP.rar": "Movie.2024.1080p.BluRay.x264-GRP|rar",
		"Movie.2024.1080p.BluRay.x264-GRP.r00": "Movie.2024.1080p.BluRay.x264-GRP|rar",
		"Movie.2024.1080p.BluRay.x264-GRP.s00": "Movie.2024.1080p.BluRay.x264-GRP|rar",
		"Show.S01E01.1080p.WEB-GRP.part01.rar": "Show.S01E01.1080p.WEB-GRP|partrar",
		// Numbered volumes keep the container extension: it is part of what
		// identifies the set, and dropping it collided with other releases.
		"Movie.2024.1080p-GRP.zip.001": "Movie.2024.1080p-GRP.zip|num",
		"Movie.2024.1080p-GRP.7z.002":  "Movie.2024.1080p-GRP.7z|num",
		"Movie.2024.1080p-GRP.001":     "Movie.2024.1080p-GRP|num",
		// Not volumes at all.
		"Movie.2024.1080p-GRP.mkv": "",
		"Movie.2024-GRP.r1":        "",
	}
	for name, want := range cases {
		if got := archiveStem(name); got != want {
			t.Errorf("archiveStem(%q) = %q, want %q", name, got, want)
		}
	}

	// Pairs that MUST NOT share a key. Each one was a measured data-loss bug or
	// is one trim away from becoming one: a shared key means CleanupArchives
	// deletes the set that was never unpacked.
	for _, p := range [][2]string{
		// Round 1: the trailing segment was eaten.
		{"Movie.2024.1080p-GRP.rar", "Movie.2024.720p-OTHER.rar"},
		// Round 2: same base, different volume form.
		{"Movie.2024-GRP.zip.001", "Movie.2024-GRP.rar"},
		// Round 3: the container strip ate the trailing segment inside .00N.
		{"Movie.2024.1080p-GRP.001", "Movie.2024.720p-OTHER.001"},
		// Round 4: "(\.partNN)?" was stripped off the base whether or not it
		// belonged to the suffix, so a release NAMED "...part01" merged with a
		// different one. Both measured: 4 files deleted for a 2-file set.
		{"Movie.001", "Movie.part01.001"},
		{"Movie.rar", "Movie.part01.r00"},
		{"Movie.part01.rar", "Movie.rar"},
		// Two different archives of the SAME release are still two sets.
		{"Movie.2024-GRP.zip.001", "Movie.2024-GRP.7z.001"},
	} {
		if a, b := archiveStem(p[0]), archiveStem(p[1]); a == b {
			t.Errorf("%q and %q share key %q — cleanup would delete both", p[0], p[1], a)
		}
	}

	// ...and the inverse: every volume of ONE set must share a key, or cleanup
	// leaves volumes behind. A rar set runs .rar → .rNN → .sNN.
	same := []string{"Movie-GRP.rar", "Movie-GRP.r00", "Movie-GRP.r99", "Movie-GRP.s00"}
	for _, n := range same[1:] {
		if archiveStem(n) != archiveStem(same[0]) {
			t.Errorf("%q (%q) does not share the key of %q (%q)",
				n, archiveStem(n), same[0], archiveStem(same[0]))
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

// Every volume of one rar set shares a key, whichever form it takes.
// (Exact key values and the non-collision pairs live in
// TestArchiveStem_KeepsFullReleaseName.)
func TestArchiveStem(t *testing.T) {
	key := archiveStem("show.rar")
	if key == "" {
		t.Fatal("show.rar is not recognised as a volume")
	}
	for _, name := range []string{"show.r00", "show.s00"} {
		if got := archiveStem(name); got != key {
			t.Errorf("archiveStem(%q) = %q, want %q (same set)", name, got, key)
		}
	}
	// A part-numbered set is its OWN set: a release ships as ".partNN.rar" or as
	// ".rar"/".rNN", never both, so two forms in one directory are two archives.
	partKey := archiveStem("show.part01.rar")
	if partKey == key {
		t.Errorf("show.part01.rar shares the plain rar set's key %q", key)
	}
	if got := archiveStem("show.part02.rar"); got != partKey {
		t.Errorf("archiveStem(%q) = %q, want %q (same part set)", "show.part02.rar", got, partKey)
	}
	if got := archiveStem("show.mkv"); got != "" {
		t.Errorf("archiveStem(%q) = %q, want \"\" (not a volume)", "show.mkv", got)
	}
	// A split of the same release is a DIFFERENT set: same base, other form.
	if got := archiveStem("show.zip.001"); got == key {
		t.Errorf("show.zip.001 shares the rar set's key %q", key)
	}
}

// BRUTE FORCE over the whole key space: for every PAIR of volume names in a
// corpus of realistic and adversarial bases, the key must match if and only if
// the two belong to the same set.
//
// This exists because three consecutive fixes to archiveStem each looked
// obviously correct, passed the suite, and each introduced a NEW collision that
// deleted a user's files. Hand-picked cases kept missing the next one; a
// property over all pairs does not.
//
// The two failure modes, both checked:
//   - COLLISION: different sets share a key → CleanupArchives deletes the set
//     that was never unpacked. DATA LOSS.
//   - LEAK: volumes of one set get different keys → volumes survive cleanup.
func TestArchiveStem_NoCollisionsOverCorpus(t *testing.T) {
	// Names are listed EXPLICITLY with the set they belong to, not generated by
	// crossing bases with suffixes. Generation looked thorough and was subtly
	// wrong: "Movie.part01" + ".rar" and "Movie" + ".part01.rar" produce the
	// same filename from different labels, so the corpus argued with itself
	// instead of testing the parser.
	//
	// A set id is arbitrary; only the equivalence classes matter. Same id ⇒ same
	// key required; different id ⇒ different key required.
	sets := map[string][]string{
		// One release, classic rar form: .rar → .rNN → .sNN.
		"movie/rar": {"Movie.rar", "Movie.r00", "Movie.r01", "Movie.r99", "Movie.s00"},
		// The SAME release in part-numbered form is a DIFFERENT archive: a
		// release ships as one form or the other, so both present means two.
		"movie/partrar": {"Movie.part01.rar", "Movie.part02.rar", "Movie.part10.rar"},
		// ...and a numbered split of it is a third.
		"movie/num": {"Movie.001", "Movie.002", "Movie.003"},
		// Container-wrapped splits: the container identifies the archive, so zip
		// and 7z of one release are two sets.
		"movie/zip": {"Movie.zip.001", "Movie.zip.002"},
		"movie/7z":  {"Movie.7z.001", "Movie.7z.002"},
		// Scene names differing only in the LAST segment — round 1's bug.
		"scene1080/rar": {"Movie.2024.1080p-GRP.rar", "Movie.2024.1080p-GRP.r00"},
		"scene720/rar":  {"Movie.2024.720p-OTHER.rar", "Movie.2024.720p-OTHER.r00"},
		"scene1080/num": {"Movie.2024.1080p-GRP.001", "Movie.2024.1080p-GRP.002"},
		"scene720/num":  {"Movie.2024.720p-OTHER.001", "Movie.2024.720p-OTHER.002"},
		// A release whose NAME ends in something shaped like a volume suffix.
		// "Movie.part01.001" is the numbered split of the release "Movie.part01"
		// — round 4's collision was merging it into "Movie.001".
		"basePart01/num": {"Movie.part01.001", "Movie.part01.002"},
		"basePart01/rar": {"Movie.part01.r00", "Movie.part01.s00"},
		"baseRar/num":    {"Movie.rar.001", "Movie.rar.002"},
		"baseR00/num":    {"Movie.r00.001", "Movie.r00.002"},
		"baseZip/rar":    {"Movie.zip.rar", "Movie.zip.r00"},
		// Other releases entirely.
		"show1/rar":   {"Show.S01E01.rar", "Show.S01E01.r00"},
		"show2/rar":   {"Show.S01E02.rar", "Show.S01E02.r00"},
		"unicode/rar": {"Película.2024.rar", "Película.2024.r00"},
		"spaces/rar":  {"Movie with spaces.rar", "Movie with spaces.r00"},
		"pipe/rar":    {"Movie|num.rar", "Movie|num.r00"},
		"pipe2/num":   {"Movie|rar.001", "Movie|rar.002"},
		"short/rar":   {"A.rar", "A.r00"},
		"tar/num":     {"Movie.tar.001", "Movie.tar.002"},
		"targz/num":   {"Backup.tar.gz.001", "Backup.tar.gz.002"},
	}

	type member struct{ name, set string }
	var corpus []member
	for set, names := range sets {
		for _, n := range names {
			corpus = append(corpus, member{n, set})
		}
	}

	for i := range corpus {
		for j := i + 1; j < len(corpus); j++ {
			a, b := corpus[i], corpus[j]
			ka, kb := archiveStem(a.name), archiveStem(b.name)
			if ka == "" || kb == "" {
				t.Errorf("%q or %q not recognised as a volume (%q / %q)", a.name, b.name, ka, kb)
				continue
			}
			switch {
			case a.set == b.set && ka != kb:
				t.Errorf("LEAK: %q and %q are one set but key %q vs %q — volumes survive cleanup",
					a.name, b.name, ka, kb)
			case a.set != b.set && ka == kb:
				t.Errorf("COLLISION: %q and %q are different sets but share key %q — cleanup would delete both",
					a.name, b.name, ka)
			}
		}
	}
}

// Names that are legal but odd. None may key onto a different release, and the
// parser must not panic or mis-split. Uppercase DOES share a key with lowercase
// on purpose: a real rar set mixes cases (.rar + .R00), so splitting them would
// leak volumes — and two files differing only in case is a pathological case
// where deleting both is the defensible behaviour.
func TestArchiveStem_PathologicalNames(t *testing.T) {
	cases := map[string]string{
		"Movie.rar.rar":           "Movie.rar|rar",
		"Movie.r00.r00":           "Movie.r00|rar",
		"Movie.001.001":           "Movie.001|num",
		"Movie.part01.part02.rar": "Movie.part01|partrar",
		"Movie.zip.rar":           "Movie.zip|rar",
		"Movie.rar.001":           "Movie.rar|num",
		"Movie.tar.gz.001":        "Movie.tar.gz|num",
		"Movie|num.rar":           "Movie|num|rar", // separator cannot be forged
		// Not volumes at all: cleanup must never consider them.
		"001":            "",
		"":               "",
		"Movie.r1":       "",
		"Movie.r000":     "",
		"Movie.0001":     "",
		"Movie.2024.mkv": "",
	}
	for name, want := range cases {
		if got := archiveStem(name); got != want {
			t.Errorf("archiveStem(%q) = %q, want %q", name, got, want)
		}
	}
	if archiveStem("Movie.RAR") != archiveStem("Movie.rar") {
		t.Error("case-variant volumes of one set must share a key")
	}
}

// archiveVolumesOf requires BOTH a matching key AND isArchiveFile. A name the
// two disagree on is either a leak (grouped but never deleted) or an unexpected
// deletion, so the two predicates must classify identically.
func TestArchiveStem_AgreesWithIsArchiveFile(t *testing.T) {
	for _, name := range []string{
		"Movie.rar", "Movie.r00", "Movie.s00", "Movie.part01.rar",
		"Movie.001", "Movie.zip.001", "Movie.7z.002", "Movie.tar.001", "Movie.999",
		"Movie.mkv", "Movie.nfo", "Movie.r1", "Movie.0001", "Movie.en.srt",
	} {
		isVolume := archiveStem(name) != ""
		if isVolume != isArchiveFile(name) {
			t.Errorf("%q: archiveStem says volume=%v, isArchiveFile says %v",
				name, isVolume, isArchiveFile(name))
		}
	}
}

// REGRESSION for round 4, through the REAL deletion path: findFirstRarInDir
// picks the entry volume and archiveVolumesOf derives what CleanupArchives will
// remove. A directory holding a part-numbered set and a plain rar set of the
// same release used to yield all four files for a two-file set.
func TestArchiveVolumesOf_PartAndPlainSetsCoexist(t *testing.T) {
	dir := t.TempDir()
	part := []string{"Movie.part01.rar", "Movie.part02.rar"}
	plain := []string{"Movie.rar", "Movie.r00"}
	for _, n := range append(append([]string{}, part...), plain...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("Rar!\x1a\x07\x00"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The entry volume is chosen by production code, not by the test.
	entry := findFirstRarInDir(dir)
	if filepath.Base(entry) != "Movie.part01.rar" {
		t.Fatalf("entry volume = %q, want Movie.part01.rar", filepath.Base(entry))
	}

	got := archiveVolumesOf(dir, entry)
	if len(got) != len(part) {
		var names []string
		for _, g := range got {
			names = append(names, filepath.Base(g))
		}
		t.Fatalf("would delete %d files %v, want only the %d of the entered set",
			len(got), names, len(part))
	}
	for _, g := range got {
		if !strings.Contains(filepath.Base(g), ".part") {
			t.Errorf("deletion set reaches outside its own set: %s", filepath.Base(g))
		}
	}
}
