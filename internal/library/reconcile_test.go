package library

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSized(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeVideoWithMarker writes a video of the given size whose head bytes carry a
// marker, so two files differ in fingerprint unless they share size AND marker.
func writeVideoWithMarker(t *testing.T, path string, size int, marker byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, size)
	for i := 0; i < 4096 && i < size; i++ {
		buf[i] = marker
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileDryRunAndApply (RC-4) seeds a tmp download dir with every anomaly
// class plus one healthy video, and asserts: dry-run lists them all and touches
// nothing; --apply removes them and leaves the healthy video.
func TestReconcileDryRunAndApply(t *testing.T) {
	root := t.TempDir()

	stub := filepath.Join(root, "stub.mkv")
	writeSized(t, stub, 512) // < 1 MiB → stub

	partial := filepath.Join(root, "downloading.part")
	writeSized(t, partial, 2048)

	orphanSub := filepath.Join(root, "loose", "orphan.srt")
	writeSized(t, orphanSub, 100) // no video in its dir

	emptyDir := filepath.Join(root, "emptyshow")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mediaNamedDir := filepath.Join(root, "movie.mkv")
	if err := os.MkdirAll(mediaNamedDir, 0o755); err != nil {
		t.Fatal(err)
	}

	healthy := filepath.Join(root, "Good Movie (2020)", "Good Movie (2020).mkv")
	writeSized(t, healthy, 2*1024*1024) // >= 1 MiB → real video

	paths := ReconcilePaths{DownloadDir: root}
	opts := DefaultReconcileOptions()

	// Dry run: report everything, touch nothing.
	findings, err := Reconcile(paths, nil, opts)
	if err != nil {
		t.Fatalf("dry-run reconcile: %v", err)
	}
	if len(findings) < 5 {
		t.Errorf("dry-run found %d anomalies, want >= 5 (stub, partial, sub, empty dir, media-named dir)", len(findings))
	}
	for _, p := range []string{stub, partial, orphanSub, emptyDir, mediaNamedDir, healthy} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("dry-run must not remove %s: %v", p, err)
		}
	}

	// Apply: remove anomalies, keep the healthy video.
	opts.Apply = true
	if _, err := Reconcile(paths, nil, opts); err != nil {
		t.Fatalf("apply reconcile: %v", err)
	}
	for _, p := range []string{stub, partial, orphanSub, emptyDir, mediaNamedDir} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("apply must remove %s (err=%v)", p, err)
		}
	}
	if _, err := os.Stat(healthy); err != nil {
		t.Errorf("apply must keep the healthy video: %v", err)
	}
}

// TestReconcileActivePartialProtected asserts a partial listed as active is not
// reaped even in apply mode.
func TestReconcileActivePartialProtected(t *testing.T) {
	root := t.TempDir()
	partial := filepath.Join(root, "live.part")
	writeSized(t, partial, 4096)

	active := map[string]bool{filepath.Clean(partial): true}
	opts := DefaultReconcileOptions()
	opts.Apply = true

	if _, err := Reconcile(ReconcilePaths{DownloadDir: root}, active, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Errorf("an active partial must be protected: %v", err)
	}
}

// TestReconcileDedupExact (RC-8) is the core dedup test: a dir with 3 byte-identical
// copies of an episode + 1 genuinely different version → 2 copies removed, the
// canonical name kept, and the distinct version untouched.
func TestReconcileDedupExact(t *testing.T) {
	root := t.TempDir()
	seasonDir := filepath.Join(root, "TV", "Baki", "Season 01")

	const size = 3 * 1024 * 1024
	canonical := filepath.Join(seasonDir, "Baki - S01E03.mkv")
	dupA := filepath.Join(seasonDir, "Baki - S01E03 (2).mkv")
	dupB := filepath.Join(seasonDir, "Baki - S01E03 [torrent].mkv")
	// 3 identical copies (same size, same marker → same fingerprint).
	writeVideoWithMarker(t, canonical, size, 'A')
	writeVideoWithMarker(t, dupA, size, 'A')
	writeVideoWithMarker(t, dupB, size, 'A')
	// A genuinely different version: same size but different content (marker 'B').
	distinct := filepath.Join(seasonDir, "Baki - S01E03 [2160p].mkv")
	writeVideoWithMarker(t, distinct, size, 'B')

	paths := ReconcilePaths{TVShowsDir: filepath.Join(root, "TV")}
	opts := DefaultReconcileOptions()
	opts.Apply = true

	findings, err := Reconcile(paths, nil, opts)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var dupCount int
	for _, f := range findings {
		if f.Kind == KindDuplicate {
			dupCount++
		}
	}
	if dupCount != 2 {
		t.Errorf("expected 2 duplicate findings, got %d", dupCount)
	}

	// Canonical (no suffix) is kept; the two suffixed identical copies are gone.
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("canonical copy must be kept: %v", err)
	}
	if _, err := os.Stat(dupA); !os.IsNotExist(err) {
		t.Errorf("dup (2) should have been removed")
	}
	if _, err := os.Stat(dupB); !os.IsNotExist(err) {
		t.Errorf("dup [torrent] should have been removed")
	}
	// The distinct-content version is NEVER removed.
	if _, err := os.Stat(distinct); err != nil {
		t.Errorf("distinct version must be kept (different fingerprint): %v", err)
	}
}

// TestReconcileUnarrSidecarNotOrphan asserts the parent-dir logic: a per-track
// .unarr/*.vtt is NOT orphaned when the parent release still holds the video, but
// IS orphaned when the release has no video (only junk).
func TestReconcileUnarrSidecarNotOrphan(t *testing.T) {
	root := t.TempDir()

	// Release WITH video: its .unarr sidecar must be kept.
	withVideo := filepath.Join(root, "Show.S01E01")
	writeSized(t, filepath.Join(withVideo, "Show.S01E01.mkv"), 2*1024*1024)
	keptSub := filepath.Join(withVideo, ".unarr", "track0.vtt")
	writeSized(t, keptSub, 200)

	// Release WITHOUT video (moved to TV Shows): only .nfo + .unarr remain →
	// the .unarr sidecar IS orphaned.
	noVideo := filepath.Join(root, "Show.S01E02")
	writeSized(t, filepath.Join(noVideo, "release.nfo"), 50)
	orphanSub := filepath.Join(noVideo, ".unarr", "track0.vtt")
	writeSized(t, orphanSub, 200)

	opts := DefaultReconcileOptions()
	opts.Apply = true
	if _, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(keptSub); err != nil {
		t.Errorf(".unarr sidecar of a release WITH video must be kept: %v", err)
	}
	if _, err := os.Stat(orphanSub); !os.IsNotExist(err) {
		t.Errorf(".unarr sidecar of a video-less release must be removed as orphan")
	}
}

// TestReconcileDedupOnly asserts --dedup-only skips the other categories.
func TestReconcileDedupOnly(t *testing.T) {
	root := t.TempDir()
	stub := filepath.Join(root, "stub.mkv")
	writeSized(t, stub, 512)

	opts := DefaultReconcileOptions()
	opts.DedupOnly = true
	opts.Apply = true

	findings, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Errorf("dedup-only should not flag a stub, got %d findings", len(findings))
	}
	if _, err := os.Stat(stub); err != nil {
		t.Errorf("dedup-only must not remove a stub: %v", err)
	}
}
