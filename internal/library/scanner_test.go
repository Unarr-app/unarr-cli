package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// videoFixture writes a file of `size` bytes that stands in for a real video:
// a NON-ZERO first MiB (a plausible container header) backed by real allocated
// blocks.
//
// A plain `make([]byte, size)` no longer works as a fixture, and that is the
// point: discoverFiles skips zero-content stubs, because a file whose header is
// all NUL has no container magic and no demuxer can open it (see
// isZeroContentStub). Every fixture here used to be exactly that shape, so
// after the gate landed these tests discovered nothing and passed for the
// wrong reason.
//
// The non-zero run covers a whole fpChunk rather than just headerProbe, so the
// fixture stays valid if the probe window ever grows.
func videoFixture(t *testing.T, path string, size int) {
	t.Helper()
	buf := make([]byte, size)
	for i := 0; i < fpChunk && i < size; i++ {
		buf[i] = 0x1A
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestDiscoverFiles(t *testing.T) {
	dir := t.TempDir()

	// Video files need to be >= 100MB to pass the size check.
	const largeSize = 101 * 1024 * 1024

	videoFiles := []string{"movie.mkv", "show.mp4", "clip.avi"}
	for _, name := range videoFiles {
		videoFixture(t, filepath.Join(dir, name), largeSize)
	}

	// Non-video files (should be excluded)
	nonVideo := []string{"readme.txt", "cover.jpg", "subs.srt"}
	for _, name := range nonVideo {
		videoFixture(t, filepath.Join(dir, name), largeSize)
	}

	// Small video file (should be excluded, < 100MB)
	if err := os.WriteFile(filepath.Join(dir, "small.mkv"), []byte("small"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Excluded pattern (sample)
	sampleDir := filepath.Join(dir, "sample")
	os.MkdirAll(sampleDir, 0o755)
	videoFixture(t, filepath.Join(sampleDir, "sample.mkv"), largeSize)

	files, err := discoverFiles(dir)
	if err != nil {
		t.Fatalf("discoverFiles: %v", err)
	}

	if len(files) != 3 {
		t.Errorf("expected 3 files, got %d: %v", len(files), files)
	}

	// Check that all returned files are video extensions
	for _, f := range files {
		ext := filepath.Ext(f)
		if ext != ".mkv" && ext != ".mp4" && ext != ".avi" {
			t.Errorf("unexpected extension: %s", ext)
		}
	}
}

func TestDiscoverFilesEmptyDir(t *testing.T) {
	dir := t.TempDir()

	files, err := discoverFiles(dir)
	if err != nil {
		t.Fatalf("discoverFiles: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files, got %d", len(files))
	}
}

func TestDiscoverFilesExcludePatterns(t *testing.T) {
	dir := t.TempDir()

	excludeDirs := []string{"trailer", "featurette", "extras", "bonus"}
	for _, name := range excludeDirs {
		sub := filepath.Join(dir, name)
		os.MkdirAll(sub, 0o755)
		// A REAL video in each excluded dir: with a zero-content stub the test
		// would pass on the stub gate instead of on the exclude patterns.
		videoFixture(t, filepath.Join(sub, "video.mkv"), 101*1024*1024)
	}

	files, err := discoverFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files (all excluded), got %d: %v", len(files), files)
	}
}

// A scan whose context is cancelled must (a) stop spawning probes and (b) report
// an error instead of returning a partial cache as if it were complete.
//
// Both halves are the 2026-07-21 incident. The loop used `break` inside a
// `select`, which only exits the SELECT — so after cancellation it kept
// launching a probe per remaining file, each failing instantly with
// "context canceled", and BuildSyncItems synced every one as damaged/
// "unreadable". And returning (cache, nil) let runAutoScan claim fullCycle on a
// truncated scan, whose stale-cleanup DELETEs every row the scan never reached.
func TestScanCancelledContextFailsInsteadOfFlaggingFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.mkv", "b.mkv", "c.mkv", "d.mkv", "e.mkv"} {
		// Real bytes, not a truncate: a sparse stub over the discovery floor is
		// now skipped as zero-content, so this test would have gone on passing
		// while probing nothing at all.
		videoFixture(t, filepath.Join(dir, name), minFileSize+1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead before the first iteration

	cache, err := Scan(ctx, dir, nil, ScanOptions{Workers: 2})
	if err == nil {
		t.Fatalf("cancelled scan returned nil error (caller would claim fullCycle); cache=%+v", cache)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error should wrap context.Canceled, got %v", err)
	}
	if cache != nil {
		t.Errorf("cancelled scan must not return a cache, got %d items", len(cache.Items))
	}
}
