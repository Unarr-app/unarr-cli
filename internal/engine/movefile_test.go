package engine

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// captureLog redirects the standard logger for the duration of fn.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	fn()
	return buf.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestMoveFileSameFilesystemRenames(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	dst := filepath.Join(dir, "dst.mkv")
	writeFile(t, src, "payload")

	if err := moveFile(src, dst); err != nil {
		t.Fatalf("moveFile: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source still present after a move: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "payload" {
		t.Errorf("destination = %q, %v; want %q", got, err, "payload")
	}
}

// The failure this whole file exists for: the copy succeeds, the delete does
// not, and the release ends up in both places. It must be reported, and the
// move must still be reported as done — the destination is correct.
func TestMoveFileReportsAnUndeletableSource(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	base := t.TempDir()
	srcDir := filepath.Join(base, "src")
	if err := os.Mkdir(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(srcDir, "movie.mkv")
	dst := filepath.Join(base, "movie.mkv")
	writeFile(t, src, "payload")

	// Force the copy path (rename would succeed here) by making rename fail:
	// a read-only parent directory refuses both rename-away and unlink, which
	// is the same shape as Windows' ERROR_SHARING_VIOLATION on delete.
	//
	// THE SCENARIO IS REAL ON WINDOWS; THE WAY THIS PROVOKES IT IS NOT. A
	// read-only directory bit does not stop a delete there — Windows takes that
	// decision from the ACL, not from the mode Go maps onto it — so the source
	// is removed, the fixture collapses, and the test fails while reporting
	// nothing about moveFile. Provoking it natively would mean holding an open
	// handle without FILE_SHARE_DELETE, which Go's os package does not offer.
	if runtime.GOOS == "windows" {
		t.Skip("chmod cannot make a directory undeletable on Windows; the fixture, not the behaviour, is unix-only")
	}
	if err := os.Chmod(srcDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(srcDir, 0o755) })

	out := captureLog(t, func() {
		if err := moveFile(src, dst); err != nil {
			t.Fatalf("moveFile returned an error, but the destination is complete: %v", err)
		}
	})

	if got, err := os.ReadFile(dst); err != nil || string(got) != "payload" {
		t.Fatalf("destination = %q, %v; want %q", got, err, "payload")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("this test needs the source to survive; it did not: %v", err)
	}
	if !strings.Contains(out, "could not remove the source") {
		t.Errorf("the duplicate was not reported; log was:\n%s", out)
	}
}

func TestCopyFileLeavesNothingBehindWhenItCannotFinish(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst.mkv")

	// A directory as the source: Open succeeds, io.Copy fails (EISDIR).
	srcDir := filepath.Join(dir, "adir")
	if err := os.Mkdir(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(srcDir, dst); err == nil {
		t.Fatal("copyFile succeeded on a directory source")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("a partial destination was left at %s (%v) — it would look like a truncated video in the library", dst, err)
	}
}

func TestCopyFileMissingSource(t *testing.T) {
	dir := t.TempDir()
	if err := copyFile(filepath.Join(dir, "nope.mkv"), filepath.Join(dir, "dst.mkv")); err == nil {
		t.Fatal("copyFile succeeded with no source file")
	}
}
