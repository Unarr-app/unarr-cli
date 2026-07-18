package postprocess

import (
	"bytes"
	"crypto/sha256"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestProcess_RealPar2LazyRepairEndToEnd exercises the full lazy-parity flow
// against the REAL par2 binary: an index-only verify detects damage ("repair
// is not possible" — no recovery blocks local yet), FetchParity stages the
// volumes in, the re-verify reports repairable, and repair restores the exact
// original bytes. Skipped when par2cmdline isn't installed.
func TestProcess_RealPar2LazyRepairEndToEnd(t *testing.T) {
	if !Par2Available() {
		t.Skip("par2 binary not installed")
	}

	dir := t.TempDir()
	staging := t.TempDir()

	// Deterministic pseudo-random content — compressible zeros would let par2
	// pack too few blocks to make the test meaningful.
	content := make([]byte, 256*1024)
	rand.New(rand.NewSource(42)).Read(content)
	vid := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(vid, content, 0o644); err != nil {
		t.Fatal(err)
	}
	origSum := sha256.Sum256(content)

	// Real parity: index + recovery volumes.
	cmd := exec.Command("par2", "create", "-r20", "release.par2", "movie.mkv")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("par2 create: %v\n%s", err, out)
	}

	// Stage the recovery volumes OUT of the dir — the engine's lazy flow only
	// has the index on disk until FetchParity runs.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var staged []string
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Name()), ".vol") && strings.HasSuffix(strings.ToLower(e.Name()), ".par2") {
			if err := os.Rename(filepath.Join(dir, e.Name()), filepath.Join(staging, e.Name())); err != nil {
				t.Fatal(err)
			}
			staged = append(staged, e.Name())
		}
	}
	if len(staged) == 0 {
		t.Fatal("par2 create produced no recovery volumes")
	}

	// Corrupt the middle of the file.
	f, err := os.OpenFile(vid, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	garbage := bytes.Repeat([]byte{0xFF}, 4096)
	if _, err := f.WriteAt(garbage, int64(len(content)/2)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	files := map[string]string{
		"movie.mkv":    vid,
		"release.par2": filepath.Join(dir, "release.par2"),
	}

	fetchCalls := 0
	res, err := Process(dir, files, Options{FetchParity: func() (map[string]string, error) {
		fetchCalls++
		out := make(map[string]string)
		for _, name := range staged {
			dst := filepath.Join(dir, name)
			if err := os.Rename(filepath.Join(staging, name), dst); err != nil {
				return nil, err
			}
			out[name] = dst
		}
		return out, nil
	}})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if fetchCalls != 1 {
		t.Errorf("FetchParity calls = %d, want 1", fetchCalls)
	}
	if res.Corrupt {
		t.Fatalf("Corrupt = true, want repaired delivery (note: %s)", res.VerifyNote)
	}
	if !res.Repaired {
		t.Fatal("Repaired = false, want true (real par2 repair)")
	}

	repaired, err := os.ReadFile(vid)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(repaired) != origSum {
		t.Fatal("repaired file content does not match the original")
	}
}
