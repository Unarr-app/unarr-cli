package engine

import (
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/library"
)

// TestVideoExtParity is the guard for the data-loss bug where engine.isVideoFile
// and library.videoExts recognised DIFFERENT extension sets: engine had .m2ts but
// not .mpg/.mpeg/.vob, library had the inverse. reconcile keys off library, so a
// .m2ts Blu-ray remux was judged a non-video → its dir was flagged empty_dir →
// RemoveAll deleted a legitimate 30 GB film.
//
// Both now resolve through the SAME canonical set (library.IsVideoExt, which
// engine.isVideoFile delegates to). This test asserts they agree on every
// container we care about — if a future edit reintroduces a divergent list here,
// it fails.
func TestVideoExtParity(t *testing.T) {
	// The full superset every subsystem must recognise. A regression that drops any
	// of these from the canonical set (or reintroduces a private list in engine)
	// makes isVideoFile disagree with library and fails this test.
	superset := []string{
		".mkv", ".mp4", ".avi", ".wmv", ".mov", ".flv", ".webm",
		".m4v", ".ts", ".m2ts", ".mpg", ".mpeg", ".vob",
	}
	for _, ext := range superset {
		name := "movie" + ext
		if !isVideoFile(name) {
			t.Errorf("engine.isVideoFile(%q) = false, want true (canonical video ext)", name)
		}
		if !library.IsVideoExt(name) {
			t.Errorf("library.IsVideoExt(%q) = false, want true (canonical video ext)", name)
		}
		// The two MUST agree — this is the parity invariant.
		if isVideoFile(name) != library.IsVideoExt(name) {
			t.Errorf("parity broken for %q: engine=%v library=%v", name, isVideoFile(name), library.IsVideoExt(name))
		}
	}

	// Case-insensitivity: uppercase extensions must match too.
	for _, name := range []string{"FILM.MKV", "clip.M2TS", "movie.VOB"} {
		if !isVideoFile(name) || !library.IsVideoExt(name) {
			t.Errorf("case-insensitive match failed for %q (engine=%v library=%v)",
				name, isVideoFile(name), library.IsVideoExt(name))
		}
	}

	// Non-videos must be rejected by both, identically.
	for _, name := range []string{"movie.srt", "poster.jpg", "readme.txt", "archive.zip", "noext"} {
		if isVideoFile(name) != library.IsVideoExt(name) {
			t.Errorf("parity broken for non-video %q: engine=%v library=%v",
				name, isVideoFile(name), library.IsVideoExt(name))
		}
		if isVideoFile(name) {
			t.Errorf("isVideoFile(%q) = true, want false", name)
		}
	}
}
