package engine

import (
	"os"
	"path/filepath"
	"testing"
)

const fpChunk = 1 << 20 // mirrors library.fpChunk (1 MiB) for crafting collisions

// writeHMT writes totalSize bytes: first/last fpChunk = head/tail, middle = mid.
func writeHMT(t *testing.T, path string, totalSize int, head, mid, tail byte) {
	t.Helper()
	buf := make([]byte, totalSize)
	for i := range buf {
		buf[i] = mid
	}
	for i := 0; i < fpChunk && i < totalSize; i++ {
		buf[i] = head
		buf[totalSize-1-i] = tail
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSameContentFullCompare is the #3 organize-side regression: two files with a
// colliding fingerprint (same extremes, different middle) must NOT be judged the
// same content, so organize never removes a distinct file as "redundant". Two
// truly identical files must be judged same.
func TestSameContentFullCompare(t *testing.T) {
	dir := t.TempDir()
	const size = 3 * fpChunk
	a := filepath.Join(dir, "a.mkv")
	b := filepath.Join(dir, "b.mkv")
	c := filepath.Join(dir, "c.mkv")
	writeHMT(t, a, size, 'H', 'X', 'T')
	writeHMT(t, b, size, 'H', 'Y', 'T') // colliding fingerprint, different middle
	writeHMT(t, c, size, 'H', 'X', 'T') // identical to a

	if sameContent(a, b) {
		t.Error("sameContent(a,b) = true, but middles differ — full compare must reject")
	}
	if !sameContent(a, c) {
		t.Error("sameContent(a,c) = false, but the files are identical")
	}
	// Different sizes short-circuit.
	small := filepath.Join(dir, "small.mkv")
	os.WriteFile(small, []byte("tiny"), 0o644)
	if sameContent(a, small) {
		t.Error("sameContent for different sizes must be false")
	}
}
