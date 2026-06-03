package library

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func fp(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	s, err := ComputeFingerprint(path, fi.Size())
	if err != nil {
		t.Fatalf("fingerprint %s: %v", path, err)
	}
	return s
}

func TestComputeFingerprint(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, 5<<20) // 5 MiB > 2*chunk
	for i := range big {
		big[i] = byte(i * 7)
	}

	a := fp(t, writeFile(t, dir, "a.bin", big))
	if len(a) != 64 {
		t.Fatalf("want 64-hex, got %d", len(a))
	}

	// Move-invariance: identical bytes at a different path → same fingerprint.
	if b := fp(t, writeFile(t, dir, "moved.bin", big)); b != a {
		t.Errorf("move changed fingerprint: %s != %s", a, b)
	}

	// Tail sensitivity: flipping the last byte must change the fingerprint.
	tailMut := append([]byte(nil), big...)
	tailMut[len(tailMut)-1] ^= 0xFF
	if c := fp(t, writeFile(t, dir, "tail.bin", tailMut)); c == a {
		t.Error("tail mutation did not change fingerprint")
	}

	// Head sensitivity.
	headMut := append([]byte(nil), big...)
	headMut[0] ^= 0xFF
	if c := fp(t, writeFile(t, dir, "head.bin", headMut)); c == a {
		t.Error("head mutation did not change fingerprint")
	}

	// Size is mixed in: a small file and a large file never collide trivially.
	small := fp(t, writeFile(t, dir, "small.bin", []byte("hello world")))
	if small == a {
		t.Error("small and big fingerprints collided")
	}
}

func TestRelToRoot(t *testing.T) {
	cases := []struct{ root, full, want string }{
		{"/downloads", "/downloads/TV Shows/X/S01E09.mkv", "TV Shows/X/S01E09.mkv"},
		{"/downloads", "/mnt/other/file.mkv", ""}, // outside root
		{"/downloads", "/downloads", ""},          // equal → "."
		{"", "/x/y.mkv", ""},                      // no root
	}
	for _, c := range cases {
		if got := relToRoot(c.root, c.full); got != c.want {
			t.Errorf("relToRoot(%q,%q)=%q want %q", c.root, c.full, got, c.want)
		}
	}
}
