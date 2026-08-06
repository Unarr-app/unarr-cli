package postprocess

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The preference order (native first, shell as a rescue) is a decision that can
// only be reversed on evidence, so these tests pin both halves of it: the native
// path is used when it works, and the shell path really does run when it does not.

// TestExtract_PrefersNative proves the shell extractors are not consulted at all
// on the happy path — a machine WITH unrar/7z installed (every dev box, CI
// included) must still go through the native decoder.
func TestExtract_PrefersNative(t *testing.T) {
	var nativeCalled bool
	orig := extractNativeFn
	extractNativeFn = func(archive, outputDir, password string) ([]string, error) {
		nativeCalled = true
		return orig(archive, outputDir, password)
	}
	t.Cleanup(func() { extractNativeFn = orig })

	dest := t.TempDir()
	files, err := Extract(fixture(t, "rar5-subdirs.rar"), dest, "")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !nativeCalled {
		t.Error("native extractor was not used")
	}
	if len(files) != 4 {
		t.Errorf("extracted %d files, want 4", len(files))
	}
}

// TestExtract_FallsBackToShell covers the safety net: when the native decoder
// chokes on something a shell extractor can open, the release is still
// delivered rather than lost.
//
// The native path is forced to fail because a real archive that rardecode
// rejects and unrar accepts is precisely what we do not have a sample of — that
// is the whole reason the fallback exists rather than being deleted.
func TestExtract_FallsBackToShell(t *testing.T) {
	if extType, _ := FindExtractor(); extType == ExtractorNone {
		t.Skip("no unrar/7z installed; nothing to fall back to")
	}

	orig := extractNativeFn
	extractNativeFn = func(string, string, string) ([]string, error) {
		return nil, errors.New("simulated native decoder failure")
	}
	t.Cleanup(func() { extractNativeFn = orig })

	dest := t.TempDir()
	files, err := Extract(fixture(t, "rar5-subdirs.rar"), dest, "")
	if err != nil {
		t.Fatalf("shell fallback did not rescue the archive: %v", err)
	}
	if len(files) == 0 {
		t.Error("fallback reported success with no files")
	}
	if _, err := os.Stat(filepath.Join(dest, "sub", "dir1", "file1.txt")); err != nil {
		t.Errorf("expected member missing after fallback: %v", err)
	}
}

// TestExtract_NoFallbackForPasswordErrors pins the one case where the fallback
// is deliberately skipped: a wrong password is deterministic, so retrying
// through a second extractor only doubles the time to the same answer.
func TestExtract_NoFallbackForPasswordErrors(t *testing.T) {
	var shellWouldRun bool
	orig := extractNativeFn
	extractNativeFn = func(string, string, string) ([]string, error) {
		return nil, &PasswordError{Archive: "a.rar"}
	}
	t.Cleanup(func() { extractNativeFn = orig })

	// If the fallback ran it would have to call FindExtractor first; the archive
	// here is real, so a shell extractor WOULD succeed and mask the bug.
	dest := t.TempDir()
	_, err := Extract(fixture(t, "rar5-subdirs.rar"), dest, "")
	if err == nil {
		t.Fatal("password error was swallowed by the fallback")
	}
	var pwErr *PasswordError
	if !errors.As(err, &pwErr) {
		t.Errorf("error = %v, want *PasswordError", err)
	}
	// Nothing was extracted, i.e. no shell rescue happened behind the scenes.
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if len(entries) != 0 {
		shellWouldRun = true
	}
	if shellWouldRun {
		t.Errorf("dest is not empty (%d entries), so a shell extractor ran", len(entries))
	}
}

// TestExtract_NoExtractorInstalled covers the machine this feature targets:
// nothing in PATH, extraction must still work.
//
// FindExtractor cannot be stubbed from here (Extract calls it directly), so the
// PATH is emptied for the duration — which is a truer simulation anyway.
func TestExtract_NoExtractorInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if extType, _ := FindExtractor(); extType != ExtractorNone {
		t.Fatalf("PATH override failed, still found %s", extType)
	}

	dest := t.TempDir()
	files, err := Extract(fixture(t, "rar5-subdirs.rar"), dest, "")
	if err != nil {
		t.Fatalf("extraction without any binary failed: %v", err)
	}
	if len(files) != 4 {
		t.Errorf("extracted %d files, want 4", len(files))
	}
}

// TestExtract_ReportsNativeErrorWhenBothFail pins the error reported when
// neither path works: the native one, which describes the archive, rather than
// the shell's generic non-zero exit.
func TestExtract_ReportsNativeErrorWhenBothFail(t *testing.T) {
	dir := t.TempDir()
	bogus := filepath.Join(dir, "mystery.rar")
	if err := os.WriteFile(bogus, []byte("not an archive"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Extract(bogus, t.TempDir(), "")
	if err == nil {
		t.Fatal("want an error when nothing can extract the file")
	}
	if !errors.Is(err, errUnsupportedFormat) {
		t.Errorf("error = %v, want the native cause (errUnsupportedFormat) preserved", err)
	}
}

// TestExtractInDir_WorksWithoutAnyBinary is the regression this whole change
// exists for, at the level users experience it.
//
// Before: ExtractInDir checked FindExtractor first and, finding nothing,
// returned a Note leaving the release as raw .rNN parts for the user to unpack
// by hand. Now the release is unpacked.
func TestExtractInDir_WorksWithoutAnyBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	// A directory holding a real archive, as a finished download would look.
	dir := t.TempDir()
	src, err := os.ReadFile(fixture(t, "rar5-subdirs.rar"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "release.rar"), src, 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	res, err := ExtractInDir(dir, "")
	if err != nil {
		t.Fatalf("ExtractInDir: %v", err)
	}
	if !res.Extracted {
		t.Fatalf("Extracted = false, Note = %q", res.Note)
	}
	if len(res.Files) != 4 {
		t.Errorf("extracted %d files, want 4", len(res.Files))
	}
}

// TestExtractInDirTo_LeavesSourceUntouched pins the seeding contract: the
// torrent's directory must stay byte-for-byte identical, since the swarm is
// still being served from it.
func TestExtractInDirTo_LeavesSourceUntouched(t *testing.T) {
	src := t.TempDir()
	archive, err := os.ReadFile(fixture(t, "rar5-subdirs.rar"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "release.rar"), archive, 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	before := fileBytes(t, src)

	destDir := filepath.Join(t.TempDir(), "extracted")
	res, err := ExtractInDirTo(src, destDir, "")
	if err != nil {
		t.Fatalf("ExtractInDirTo: %v", err)
	}
	if !res.Extracted {
		t.Fatalf("Extracted = false, Note = %q", res.Note)
	}

	// Output landed in the sibling directory...
	for _, f := range res.Files {
		if !isWithin(destDir, f) {
			t.Errorf("file %s written outside destDir %s", f, destDir)
		}
	}
	// ...and the seeding directory is untouched.
	after := fileBytes(t, src)
	if len(after) != len(before) {
		t.Errorf("source file count changed: %d → %d", len(before), len(after))
	}
	for name, want := range before {
		if after[name] != want {
			t.Errorf("source file %s was modified or removed", name)
		}
	}

	// CleanupArchives must stay inert for an out-of-place extraction: the parts
	// belong to the swarm.
	if err := CleanupArchives(res); err != nil {
		t.Fatalf("CleanupArchives: %v", err)
	}
	if got := fileBytes(t, src); len(got) != len(before) {
		t.Errorf("CleanupArchives deleted from the seeding directory: %d → %d", len(before), len(got))
	}
}
