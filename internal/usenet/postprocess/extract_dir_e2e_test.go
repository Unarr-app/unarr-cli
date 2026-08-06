package postprocess

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// buildSplitArchive creates a REAL split archive with 7z and returns its dir.
// Self-contained: the fixture is generated, never checked in or borrowed from
// a scratchpad, so the test is runnable on any machine with 7z.
func buildSplitArchive(t *testing.T) string {
	t.Helper()
	sz, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z not installed")
	}

	work := t.TempDir()
	payload := filepath.Join(work, "payload.mkv")
	// 3 MB split into 1 MB volumes → .001/.002/.003.
	// The bytes must be INCOMPRESSIBLE: a run of zeros deflates to well under
	// one volume, so 7z emits a single .001 and the split never happens (which
	// is exactly how this fixture failed on first write). Deterministic PRNG
	// rather than crypto/rand so a failure reproduces byte-for-byte.
	data := make([]byte, 3_000_000)
	seed := uint32(0x9E3779B9)
	for i := range data {
		seed ^= seed << 13
		seed ^= seed >> 17
		seed ^= seed << 5
		data[i] = byte(seed)
	}
	if err := os.WriteFile(payload, data, 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(work, "release")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sz, "a", "-tzip", "-v1m",
		filepath.Join(out, "show.s01e01.zip"), payload)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not build fixture: %v\n%s", err, b)
	}
	os.Remove(payload)
	return out
}

// End-to-end over a real split archive: the packed release goes in, a playable
// file comes out, and the parts are gone.
//
// This is the regression guard for the actual user-visible bug — a torrent
// delivered as .001/.002/.003 used to reach the library as a pile of parts.
func TestExtractInDir_RealSplitArchiveE2E(t *testing.T) {
	dir := buildSplitArchive(t)

	// PREMISE: no video present before extraction — exactly the condition that
	// sent organizeDir down its move-the-raw-folder fallback.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("fixture is not split: %d file(s)", len(entries))
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".mkv" {
			t.Fatal("premise broken: fixture already exposes a video")
		}
	}

	res, err := ExtractInDir(dir, "")
	if err != nil {
		t.Fatalf("ExtractInDir: %v", err)
	}
	if !res.Extracted {
		t.Fatalf("nothing extracted (Note=%q)", res.Note)
	}

	out := filepath.Join(dir, "payload.mkv")
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatalf("payload not extracted: %v", err)
	}
	if fi.Size() != 3_000_000 {
		t.Errorf("payload size = %d, want 3000000", fi.Size())
	}

	// Drop a couple of files that a real release carries and the user cares
	// about. The old extension-sweep cleanup ate exactly these; the payload
	// being a .mkv is what made the earlier version of this test miss it.
	writeFiles(t, dir, "payload.en.srt", "poster.jpg")

	if err := CleanupArchives(res); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("cleanup removed the payload: %v", err)
	}
	for _, name := range []string{"payload.en.srt", "poster.jpg"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("cleanup removed the user's %s: %v", name, err)
		}
	}
	left, _ := os.ReadDir(dir)
	for _, e := range left {
		switch e.Name() {
		case "payload.mkv", "payload.en.srt", "poster.jpg":
		default:
			t.Errorf("archive part survived cleanup: %s", e.Name())
		}
	}
}

// A split set that is NOT rar must not be handed to unrar just because unrar is
// installed — the bug that made the E2E above fail on first run.
func TestExtract_NonRarSplitPicksCorrectExtractor(t *testing.T) {
	if _, err := exec.LookPath("unrar"); err != nil {
		t.Skip("unrar not installed; selection bug cannot manifest")
	}
	dir := buildSplitArchive(t)

	first := filepath.Join(dir, "show.s01e01.zip.001")
	if isRarArchive(first) {
		t.Fatal("fixture unexpectedly carries RAR magic")
	}

	if _, err := Extract(first, dir, ""); err != nil {
		t.Fatalf("extract of a non-rar split failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "payload.mkv")); err != nil {
		t.Errorf("payload missing after extract: %v", err)
	}
}
