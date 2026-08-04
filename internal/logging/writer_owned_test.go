package logging

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// mustStatSize fails the test when path is missing.
func mustStatSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

// TestWriterRotatesByRenameAndTheLiveFileShrinks is the whole point of the
// owned Writer: the process holding the descriptor renames its own live file
// aside and reopens the path, so the live file really does go back to zero.
// This is precisely what copy-truncate could not do on Windows, where the
// truncate was refused by cmd.exe's holder and the log grew forever.
func TestWriterRotatesByRenameAndTheLiveFileShrinks(t *testing.T) {
	path := newTestLog(t)
	w, err := NewWriter(Options{Path: path, MaxSizeMB: 1, MaxFiles: 2})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	const max = 1 << 20
	chunk := []byte(strings.Repeat("x", 64*1024))
	for i := 0; i < max/len(chunk); i++ {
		if _, werr := w.Write(chunk); werr != nil {
			t.Fatalf("write %d: %v", i, werr)
		}
	}

	if got := mustStatSize(t, path); got != 0 {
		t.Fatalf("live log is %d bytes after the rename rotation, want 0 — "+
			"the owner must have moved it aside and reopened an empty path", got)
	}
	if got := mustStatSize(t, RotatedPath(path, 1)); got != max {
		t.Fatalf("unarr.log.1 is %d bytes, want the %d that was live", got, max)
	}

	// The reopened descriptor must still be the live file, not the renamed one.
	if _, err := w.Write([]byte("after")); err != nil {
		t.Fatalf("write after rotation: %v", err)
	}
	if got := mustRead(t, path); got != "after" {
		t.Fatalf("live log is %q, want only the post-rotation write", got)
	}
}

// TestWriterSeedsItsBudgetFromTheFileOnDisk pins the restart case: a Writer
// that started its counter at zero would grant every restart a fresh budget,
// turning a 20 MB cap into 20 MB per restart.
func TestWriterSeedsItsBudgetFromTheFileOnDisk(t *testing.T) {
	path := newTestLog(t)
	const seeded = 2 * 1024 * 1024
	if err := os.WriteFile(path, []byte(strings.Repeat("o", seeded)), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w, err := NewWriter(Options{Path: path, MaxSizeMB: 1, MaxFiles: 2})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	// The constructor must not have rotated on its own — one rotation path only.
	if got := mustStatSize(t, path); got != seeded {
		t.Fatalf("live log is %d bytes straight after NewWriter, want the %d it found", got, seeded)
	}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := mustStatSize(t, path); got != 0 {
		t.Fatalf("live log is %d bytes, want 0 — the first write over an "+
			"already-oversized file must rotate it", got)
	}
	if got := mustStatSize(t, RotatedPath(path, 1)); got != seeded+1 {
		t.Fatalf("unarr.log.1 is %d bytes, want %d", got, seeded+1)
	}
}

// TestWriterKeepsTheRingBoundedAcrossManyRotations: the ring must stop growing
// and nothing may survive past the configured slot count.
func TestWriterKeepsTheRingBoundedAcrossManyRotations(t *testing.T) {
	path := newTestLog(t)
	w, err := NewWriter(Options{Path: path, MaxSizeMB: 1, MaxFiles: 2})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	const max = 1 << 20
	chunk := []byte(strings.Repeat("x", 128*1024))
	for i := 0; i < 80; i++ { // 10 budgets' worth
		if _, werr := w.Write(chunk); werr != nil {
			t.Fatalf("write %d: %v", i, werr)
		}
	}

	var total int64
	for _, p := range append([]string{path}, RotatedPaths(path, 2)...) {
		if fi, serr := os.Stat(p); serr == nil {
			total += fi.Size()
		}
	}
	if ceiling := int64(3*max + len(chunk)); total > ceiling {
		t.Fatalf("ring holds %d bytes, ceiling is %d — rotation is not bounding it", total, ceiling)
	}
	if _, err := os.Stat(RotatedPath(path, 3)); !os.IsNotExist(err) {
		t.Fatalf("unarr.log.3 exists with MaxFiles = 2 (stat err: %v)", err)
	}
}

// TestWriterWithRotationDisabledJustAppends: MaxSizeMB 0 is the documented
// escape hatch for anyone running their own logrotate.
func TestWriterWithRotationDisabledJustAppends(t *testing.T) {
	path := newTestLog(t)
	w, err := NewWriter(Options{Path: path, MaxSizeMB: 0})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	const total = 2 * 1024 * 1024
	if _, err := w.Write([]byte(strings.Repeat("x", total))); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := mustStatSize(t, path); got != total {
		t.Fatalf("live log is %d bytes, want %d — rotation is off", got, total)
	}
	if _, err := os.Stat(RotatedPath(path, 1)); !os.IsNotExist(err) {
		t.Fatal("nothing should have been rotated with MaxSizeMB = 0")
	}
}

// TestWriterSerializesConcurrentWritesAcrossRotations: the mutex must cover the
// write AND the rotation, so no line is ever torn or written into a file that
// is being renamed. Run under -race.
func TestWriterSerializesConcurrentWritesAcrossRotations(t *testing.T) {
	path := newTestLog(t)
	// MaxFiles 4 with 2.5 MiB of traffic at a 1 MiB budget: enough rotations to
	// matter, few enough that no slot is dropped and every line must survive.
	w, err := NewWriter(Options{Path: path, MaxSizeMB: 1, MaxFiles: 4})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	const (
		writers  = 8
		perWrite = 320
		lineLen  = 1024
	)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			line := []byte(strings.Repeat(string(rune('a'+id)), lineLen-1) + "\n")
			for j := 0; j < perWrite; j++ {
				if _, werr := w.Write(line); werr != nil {
					t.Errorf("writer %d: %v", id, werr)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	lines := 0
	for _, p := range append([]string{path}, RotatedPaths(path, 4)...) {
		for _, line := range strings.Split(mustRead(t, p), "\n") {
			if line == "" {
				continue
			}
			lines++
			if len(line) != lineLen-1 || strings.Count(line, line[:1]) != len(line) {
				t.Fatalf("torn line in %s: %d bytes, mixed content — writes and "+
					"rotations interleaved", p, len(line))
			}
		}
	}
	if want := writers * perWrite; lines != want {
		t.Fatalf("%d lines survived the ring, want %d", lines, want)
	}
}

// TestWriterBacksOffAndReportsOnceWhenTheRenameFails covers the degraded path:
// a reader (or an antivirus, or a CIFS share) that blocks the rename must not
// cost one shiftRotated plus one Rename PER LOG LINE, and must not flood
// anything with the same complaint. Here the block is an unwritable directory,
// which is the reproducible Linux stand-in for a Windows sharing violation.
func TestWriterBacksOffAndReportsOnceWhenTheRenameFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the rename cannot be blocked")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "unarr.log")
	w, err := NewWriter(Options{Path: path, MaxSizeMB: 1, MaxFiles: 2})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	restoreErr := captureStderr(t)
	// Read+execute only: the existing file stays writable (so the daemon keeps
	// logging) but no entry can be created or renamed inside the directory.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	const max = 1 << 20
	chunk := []byte(strings.Repeat("x", 64*1024))
	for i := 0; i < 3*max/len(chunk); i++ { // three budgets' worth
		if _, werr := w.Write(chunk); werr != nil {
			t.Fatalf("a failed rotation must not fail the write: %v", werr)
		}
	}
	stderr := restoreErr()

	if _, err := os.Stat(RotatedPath(path, 1)); !os.IsNotExist(err) {
		t.Fatalf("unarr.log.1 exists, but the rename could not have worked (stat err: %v)", err)
	}
	w.mu.Lock()
	size, rotateAt := w.size, w.rotateAt
	w.mu.Unlock()
	if rotateAt < size+max {
		t.Fatalf("rotateAt is %d with %d bytes live: a blocked rename must back "+
			"off by a full budget, not retry on every line", rotateAt, size)
	}
	if got := strings.Count(stderr, "unarr:"); got != 1 {
		t.Fatalf("stderr carries %d rotation complaints, want exactly 1:\n%s", got, stderr)
	}
	if !strings.Contains(stderr, path) {
		t.Fatalf("the complaint does not name the log file:\n%s", stderr)
	}
}

func TestNewWriterRejectsAnEmptyPath(t *testing.T) {
	if _, err := NewWriter(Options{MaxSizeMB: 1}); err == nil {
		t.Fatal("NewWriter accepted an empty path")
	}
}

func TestNewWriterCreatesTheDataDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "unarr.log")
	w, err := NewWriter(Options{Path: path, MaxSizeMB: 20})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if got := w.Path(); got != path {
		t.Fatalf("Path() is %q, want %q", got, path)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("log file not created: %v", err)
	}
}

func TestWriterCloseIsIdempotent(t *testing.T) {
	w, err := NewWriter(Options{Path: newTestLog(t), MaxSizeMB: 1})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// captureStderr redirects os.Stderr to a temp file and returns a function that
// restores it and yields what was written. The Writer reports rotation trouble
// there because it cannot report through itself.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("temp stderr: %v", err)
	}
	old := os.Stderr
	os.Stderr = f
	restored := false
	restore := func() string {
		if restored {
			return ""
		}
		restored = true
		os.Stderr = old
		_ = f.Close()
		b, rerr := os.ReadFile(f.Name())
		if rerr != nil {
			t.Fatalf("read captured stderr: %v", rerr)
		}
		return string(b)
	}
	t.Cleanup(func() { _ = restore() })
	return restore
}
