package postprocess

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Shelling out to unrar/7z used to make path safety someone else's problem:
// both refuse to write outside the destination, and 7z prints "Dangerous link
// path was ignored" for an escaping symlink (measured before this change).
// Decoding in Go moves that guarantee here, so these tests assert it directly.

// resolvedTempDir is t.TempDir() put through the same resolution newSafeWriter
// applies to its destination, and is what any containment assertion in this
// package must be given.
//
// newSafeWriter resolves destDir through symlinks ONCE and returns paths under
// that resolved base, so comparing its output against a raw t.TempDir() compares
// two spellings of the same directory. That is invisible on Linux, where the
// temp dir is already canonical, and fails on every platform where it is not:
// macOS hands out /var/folders/... while the writer resolves /var → /private/var,
// and the Windows runner hands out the 8.3 short name C:\Users\RUNNER~1\...
// which resolves to C:\Users\runneradmin\.... Both were red in CI, on a writer
// that had placed every file exactly where it promised.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

// TestSafeWriter_RejectsTraversal covers zip-slip in the form the member name
// spells out.
func TestSafeWriter_RejectsTraversal(t *testing.T) {
	tests := []struct {
		name   string
		member string
	}{
		{"parent escape", "../../../etc/passwd"},
		{"single parent", "../escaped.txt"},
		{"escape mid-path", "sub/../../escaped.txt"},
		{"absolute unix", "/etc/passwd"},
		{"drive relative", "C:evil.txt"},
		{"drive absolute", "C:/Windows/System32/evil.dll"},
		{"backslash traversal", `..\..\evil.txt`},
		{"backslash nested", `sub\..\..\evil.txt`},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dest := t.TempDir()
			w, err := newSafeWriter(dest)
			if err != nil {
				t.Fatalf("newSafeWriter: %v", err)
			}

			path, err := w.writeFile(tt.member, 0o644, strings.NewReader("pwned"))
			if err != nil {
				t.Fatalf("a refused member must not be a hard error, got %v", err)
			}
			if path != "" {
				t.Errorf("member %q was written to %q, want refusal", tt.member, path)
			}
			if len(w.skipped) != 1 {
				t.Errorf("skipped = %v, want the refusal recorded", w.skipped)
			}
		})
	}
}

// COUNTERFACTUAL for the traversal table: an ordinary nested member DOES get
// written. Without this, every assertion above would still pass if writeFile
// refused everything unconditionally.
func TestSafeWriter_WritesNormalMembers(t *testing.T) {
	dest := resolvedTempDir(t)
	w, err := newSafeWriter(dest)
	if err != nil {
		t.Fatalf("newSafeWriter: %v", err)
	}

	path, err := w.writeFile("sub/dir/movie.mkv", 0o644, strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if path == "" {
		t.Fatal("a legitimate member was refused")
	}
	if len(w.skipped) != 0 {
		t.Errorf("skipped = %v, want none", w.skipped)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("content = %q, want %q", got, "payload")
	}
	if !isWithin(dest, path) {
		t.Errorf("path %q escaped dest %q", path, dest)
	}
}

// TestSafeWriter_RejectsSymlinkMembers covers the attack a name-only check
// MISSES entirely, and which is why validating h.Name is not sufficient.
//
// The two-step: member 1 is a symlink with a perfectly innocent NAME ("link")
// whose TARGET escapes; member 2 is "link/payload", also an innocent name.
// Every path-based check passes, yet the write lands wherever the link points.
// Refusing symlink members outright removes the first step, so the second has
// nothing to traverse.
func TestSafeWriter_RejectsSymlinkMembers(t *testing.T) {
	dest := t.TempDir()
	w, err := newSafeWriter(dest)
	if err != nil {
		t.Fatalf("newSafeWriter: %v", err)
	}

	// Step 1 as it would arrive from an archive: a symlink member. Its name is
	// clean; only its MODE marks it.
	path, err := w.writeFile("link", os.ModeSymlink|0o777, strings.NewReader("../../../tmp"))
	if err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if path != "" {
		t.Errorf("symlink member was written to %q, want refusal", path)
	}
	if len(w.skipped) != 1 || !strings.Contains(w.skipped[0], "link") {
		t.Errorf("skipped = %v, want the symlink recorded", w.skipped)
	}

	// And it really is absent — the second step has nothing to walk through.
	if _, err := os.Lstat(filepath.Join(dest, "link")); !os.IsNotExist(err) {
		t.Errorf("something was created at the symlink's path: %v", err)
	}
}

// TestSafeWriter_RejectsSpecialModes covers the rest of the non-regular types.
func TestSafeWriter_RejectsSpecialModes(t *testing.T) {
	modes := map[string]os.FileMode{
		"symlink":    os.ModeSymlink | 0o777,
		"device":     os.ModeDevice | 0o644,
		"chardevice": os.ModeDevice | os.ModeCharDevice | 0o644,
		"named pipe": os.ModeNamedPipe | 0o644,
		"socket":     os.ModeSocket | 0o644,
		"setuid":     os.ModeSetuid | 0o755,
		"setgid":     os.ModeSetgid | 0o755,
		"irregular":  os.ModeIrregular | 0o644,
	}
	for name, mode := range modes {
		t.Run(name, func(t *testing.T) {
			if err := checkMode(mode); err == nil {
				t.Errorf("checkMode(%v) = nil, want refusal", mode)
			}
		})
	}

	// COUNTERFACTUAL: plain files and directories are accepted, so the table
	// above is not passing because checkMode rejects everything.
	for _, mode := range []os.FileMode{0o644, 0o755, os.ModeDir | 0o755} {
		if err := checkMode(mode); err != nil {
			t.Errorf("checkMode(%v) = %v, want accepted", mode, err)
		}
	}
}

// TestSafeWriter_RefusesToWriteThroughPlantedSymlink covers the case where the
// link was NOT planted by the archive but was already sitting in the
// destination — a leftover from an earlier run, or another process.
//
// O_NOFOLLOW is what stops it. Unix-only: Windows has no equivalent flag (see
// nofollow_windows.go for what covers the gap there).
func TestSafeWriter_RefusesToWriteThroughPlantedSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("O_NOFOLLOW has no Windows equivalent; see nofollow_windows.go")
	}

	dest := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatalf("seed victim: %v", err)
	}

	// Someone planted a link inside the destination pointing out of it.
	if err := os.Symlink(victim, filepath.Join(dest, "movie.mkv")); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	w, err := newSafeWriter(dest)
	if err != nil {
		t.Fatalf("newSafeWriter: %v", err)
	}
	// The member name is entirely innocent and resolves inside dest.
	if _, err := w.writeFile("movie.mkv", 0o644, strings.NewReader("pwned")); err == nil {
		t.Error("write through a planted symlink succeeded, want refusal")
	}

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(got) != "original" {
		t.Errorf("victim was overwritten: %q", got)
	}
}

// TestSafeWriter_AllowsSymlinkedDestination is the counterweight to the check
// above: a destination that merely SITS behind a symlink is normal (macOS
// /var → /private/var, and any user whose downloads dir is a link) and must
// keep working. A containment check that resolved only one side would reject
// every write here.
func TestSafeWriter_AllowsSymlinkedDestination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on Windows")
	}

	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "downloads")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink dest: %v", err)
	}

	w, err := newSafeWriter(link)
	if err != nil {
		t.Fatalf("newSafeWriter: %v", err)
	}
	path, err := w.writeFile("sub/movie.mkv", 0o644, strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("writeFile through symlinked dest: %v", err)
	}
	if path == "" {
		t.Fatalf("member refused; skipped = %v", w.skipped)
	}
	if _, err := os.Stat(filepath.Join(real, "sub", "movie.mkv")); err != nil {
		t.Errorf("file not written to the real destination: %v", err)
	}
}

// TestFilePerm pins the permission mapping: the executable bit survives, but
// nothing beyond 0755 does, and a 0000 member stays readable by its owner.
func TestFilePerm(t *testing.T) {
	tests := []struct {
		in   os.FileMode
		want os.FileMode
	}{
		{0o644, 0o644},
		{0o755, 0o755},
		{0o777, 0o755 | 0o600}, // world-writable is dropped
		{0o000, 0o644},         // unreadable members are made readable
		{0o400, 0o600},
	}
	for _, tt := range tests {
		if got := filePerm(tt.in); got != tt.want {
			t.Errorf("filePerm(%o) = %o, want %o", tt.in, got, tt.want)
		}
	}
}
