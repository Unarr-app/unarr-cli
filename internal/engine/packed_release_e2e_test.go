package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/usenet/postprocess"
)

// The full post-download chain over a REAL packed release: extract → organize →
// library. This is the pipeline the user actually sees, and the level at which
// the destructive bug lived: each half was correct on its own, and only their
// COMPOSITION deleted a seeding torrent's files (extractPackedRelease kept the
// parts, organize's cleanupReleaseDir then removed the directory holding them).
//
// Both seeding modes are covered because they take different paths through
// organize: in place vs sibling.
func TestPackedRelease_FullChain(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed bool
	}{
		{"seeding", true},
		{"not-seeding", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The release must live INSIDE OutputDir: cleanupReleaseDir refuses to
			// touch anything outside it, so a release elsewhere would survive for
			// the wrong reason and prove nothing.
			out := t.TempDir()
			lib := t.TempDir()
			src := buildPackedRelease(t, "show.mkv")

			releaseDir := filepath.Join(out, "Show.S01E01.1080p.WEB-GRP")
			if err := os.Rename(src, releaseDir); err != nil {
				t.Fatal(err)
			}
			partsBefore, err := os.ReadDir(releaseDir)
			if err != nil {
				t.Fatal(err)
			}

			m := &Manager{cfg: ManagerConfig{
				SeedEnabled: tc.seed,
				OutputDir:   out,
				Organize: OrganizeConfig{
					Enabled:    true,
					MoviesDir:  lib,
					TVShowsDir: lib,
					OutputDir:  out,
				},
			}}

			season := 1
			result := &Result{FilePath: releaseDir, Method: MethodTorrent}
			task := &Task{ID: "e2e-" + tc.name, ContentTitle: "Show", ContentType: "tv", Season: &season}

			m.extractPackedRelease(task, result)

			finalPath, err := organize(result, task, m.cfg.Organize)
			if err != nil {
				t.Fatalf("organize: %v", err)
			}

			// 1. THE POINT OF THE FIX: a playable video reached the library, not a
			//    pile of .001/.002 parts.
			fi, err := os.Stat(finalPath)
			if err != nil {
				t.Fatalf("nothing landed in the library: %v", err)
			}
			if fi.IsDir() {
				t.Fatalf("library got a directory (%s), not a video — release stayed packed", finalPath)
			}
			if fi.Size() != 2_000_000 {
				t.Errorf("library file size = %d, want 2000000 (truncated payload)", fi.Size())
			}
			if !isVideoFile(finalPath) {
				t.Errorf("library file %q is not a video", filepath.Base(finalPath))
			}

			// 2. The seeding contract: with seeding ON the torrent dir must survive
			//    the whole chain, byte for byte. This is what regressed before.
			if tc.seed {
				for _, e := range partsBefore {
					p := filepath.Join(releaseDir, e.Name())
					if _, err := os.Stat(p); err != nil {
						t.Errorf("seeding torrent lost %s after organize: %v", e.Name(), err)
					}
				}
			}
		})
	}
}

// A release that CANNOT be unpacked must still reach the library as the raw
// folder rather than vanish or fail the task: the parts ARE what the swarm
// served, and a download that verified fine must not be downgraded to "you get
// nothing".
//
// Uses a real password-protected archive — a recoverable failure that returns
// BEFORE result.FilePath is re-pointed, so organize must still find the original
// release dir. (An unpacked release with no video is precisely the case that
// sends organizeDir down its move-the-folder fallback.)
func TestPackedRelease_UnpackableStillReachesLibrary(t *testing.T) {
	sz, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z not installed")
	}
	out := t.TempDir()
	lib := t.TempDir()

	releaseDir := filepath.Join(out, "Show.S01E03-GRP")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	payload := filepath.Join(work, "show.mkv")
	if err := os.WriteFile(payload, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	// -p encrypts, -mhe=on also encrypts the HEADER so the archive reads as
	// password-protected rather than merely failing at extraction time.
	//
	// SPLIT (-v512k) on purpose: detection covers .rar/.rNN/.001 volumes, not a
	// single-volume .7z/.zip — a plain "show.7z" is not seen as an archive at all,
	// so this test would pass without ever reaching the password branch (measured:
	// ExtractInDir returned Extracted:false, Note:"" — nothing happened).
	cmd := exec.Command(sz, "a", "-t7z", "-psecret", "-mhe=on", "-v512k",
		filepath.Join(releaseDir, "show.7z"), payload)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not build encrypted fixture: %v\n%s", err, b)
	}
	// PREMISE: the archive really is detected AND really is password-protected.
	// Without this the assertions below would hold trivially.
	probe, probeErr := postprocess.ExtractInDir(releaseDir, "")
	if probeErr == nil {
		t.Fatalf("fixture is not password-protected: ExtractInDir returned %+v", probe)
	}
	if _, ok := probeErr.(*postprocess.PasswordError); !ok {
		t.Fatalf("want a PasswordError from the fixture, got %T: %v", probeErr, probeErr)
	}

	m := &Manager{cfg: ManagerConfig{
		SeedEnabled: true,
		OutputDir:   out,
		Organize: OrganizeConfig{
			Enabled: true, MoviesDir: lib, TVShowsDir: lib, OutputDir: out,
		},
	}}
	season := 1
	result := &Result{FilePath: releaseDir, Method: MethodTorrent}
	task := &Task{ID: "unpackable", ContentTitle: "Show", ContentType: "tv", Season: &season}

	// No password on the task: extraction cannot proceed.
	m.extractPackedRelease(task, result)

	// The result must still point at the real release: re-pointing it to a
	// sibling that was never created would strand the download.
	if result.FilePath != releaseDir {
		t.Errorf("result re-pointed to %q despite no extraction", result.FilePath)
	}
	if _, err := os.Stat(releaseDir); err != nil {
		t.Fatalf("release dir lost after a failed extraction: %v", err)
	}

	finalPath, err := organize(result, task, m.cfg.Organize)
	if err != nil {
		t.Fatalf("organize must not fail on an unpackable release: %v", err)
	}
	if _, err := os.Stat(finalPath); err != nil {
		t.Errorf("nothing reached the library: %v", err)
	}
}
