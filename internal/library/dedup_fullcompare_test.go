package library

import (
	"os"
	"path/filepath"
	"testing"
)

// writeHeadMidTail writes a file of totalSize where the first fpChunk bytes are
// `head`, the last fpChunk bytes are `tail`, and the middle is filled with `mid`.
// Used to craft a fingerprint COLLISION: two files identical on their 1 MiB
// extremes but differing in the middle share a ComputeFingerprint yet are NOT the
// same content.
func writeHeadMidTail(t *testing.T, path string, totalSize int, head, mid, tail byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, totalSize)
	for i := range buf {
		buf[i] = mid
	}
	for i := 0; i < fpChunk && i < totalSize; i++ {
		buf[i] = head
	}
	for i := 0; i < fpChunk && i < totalSize; i++ {
		buf[totalSize-1-i] = tail
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSameFileContentDistinguishesMiddle is the #3 unit: two files with identical
// size + first/last 1 MiB but a different middle collide on fingerprint yet must be
// distinguished by SameFileContent.
func TestSameFileContentDistinguishesMiddle(t *testing.T) {
	root := t.TempDir()
	const size = 3 * fpChunk // > 2*fpChunk so the fingerprint samples extremes, not the whole file
	a := filepath.Join(root, "a.mkv")
	b := filepath.Join(root, "b.mkv")
	writeHeadMidTail(t, a, size, 'H', 'X', 'T') // middle = X
	writeHeadMidTail(t, b, size, 'H', 'Y', 'T') // middle = Y

	// Sanity: the fingerprint DOES collide (proves the filter alone is insufficient).
	fpA, err := ComputeFingerprint(a, size)
	if err != nil {
		t.Fatal(err)
	}
	fpB, err := ComputeFingerprint(b, size)
	if err != nil {
		t.Fatal(err)
	}
	if fpA != fpB {
		t.Skipf("fingerprints did not collide (%s vs %s) — cannot exercise the collision path", fpA, fpB)
	}

	// The full compare must say they are DIFFERENT.
	same, err := SameFileContent(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if same {
		t.Error("SameFileContent said identical, but the middles differ (X vs Y)")
	}

	// And two genuinely identical files must compare equal.
	c := filepath.Join(root, "c.mkv")
	writeHeadMidTail(t, c, size, 'H', 'X', 'T') // same as a
	same, err = SameFileContent(a, c)
	if err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Error("SameFileContent said different for two identical files")
	}
}

// TestReconcileDedupFingerprintCollisionKeepsBoth is the #3 end-to-end: two videos
// in one dir with a colliding fingerprint but different middles must NOT be
// deduped; two truly identical ones must be.
func TestReconcileDedupFingerprintCollisionKeepsBoth(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Show", "S01")
	const size = 3 * fpChunk

	// Collision pair: same extremes, different middle → keep BOTH.
	collA := filepath.Join(dir, "Ep.mkv")
	collB := filepath.Join(dir, "Ep (2).mkv")
	writeHeadMidTail(t, collA, size, 'H', 'X', 'T')
	writeHeadMidTail(t, collB, size, 'H', 'Y', 'T')

	// Verify the collision precondition; skip if the platform/FS didn't produce it.
	fpA, _ := ComputeFingerprint(collA, size)
	fpB, _ := ComputeFingerprint(collB, size)
	if fpA != fpB {
		t.Skip("fingerprints did not collide on this platform — collision path not exercised")
	}

	opts := DefaultReconcileOptions()
	opts.Apply = true
	findings, err := Reconcile(ReconcilePaths{TVShowsDir: filepath.Join(root, "Show")}, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.Kind == KindDuplicate {
			t.Errorf("fingerprint-colliding-but-different files must NOT be deduped, flagged %s", f.Path)
		}
	}
	// Both survive.
	if _, err := os.Stat(collA); err != nil {
		t.Errorf("collA must survive: %v", err)
	}
	if _, err := os.Stat(collB); err != nil {
		t.Errorf("collB must survive: %v", err)
	}
}

// TestReconcileDedupTrulyIdenticalStillDedups guards that the full-compare
// confirmation did not break real dedup: two byte-identical copies → one removed.
func TestReconcileDedupTrulyIdenticalStillDedups(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Movie")
	const size = 3 * fpChunk
	keep := filepath.Join(dir, "Film.mkv")
	dup := filepath.Join(dir, "Film (2).mkv")
	writeHeadMidTail(t, keep, size, 'H', 'M', 'T')
	writeHeadMidTail(t, dup, size, 'H', 'M', 'T') // identical

	opts := DefaultReconcileOptions()
	opts.Apply = true
	findings, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	var dupCount int
	for _, f := range findings {
		if f.Kind == KindDuplicate {
			dupCount++
		}
	}
	if dupCount != 1 {
		t.Errorf("expected 1 duplicate (truly identical), got %d", dupCount)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("canonical must be kept: %v", err)
	}
	if _, err := os.Stat(dup); !os.IsNotExist(err) {
		t.Error("the identical dup should have been removed")
	}
}
