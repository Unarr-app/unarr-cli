package postprocess

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Native extraction tests. The RAR fixtures are vendored (see testdata/README.md
// — creating a RAR needs the proprietary compressor); 7z and zip fixtures are
// built here so nothing depends on a tool being installed.

func fixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("testdata", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture %s missing: %v", name, err)
	}
	return path
}

// TestExtractNative_RAR covers the ordinary case: nested dirs, unicode names,
// spaces. This is what the whole feature exists to do without a binary.
func TestExtractNative_RAR(t *testing.T) {
	dest := resolvedTempDir(t)
	files, err := extractNative(fixture(t, "rar5-subdirs.rar"), dest, "")
	if err != nil {
		t.Fatalf("extractNative: %v", err)
	}
	if len(files) != 4 {
		t.Fatalf("extracted %d files, want 4: %v", len(files), files)
	}

	// Every promised path exists, holds content, and lives inside dest.
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			t.Errorf("reported file %s does not exist: %v", f, err)
			continue
		}
		if st.Size() == 0 {
			t.Errorf("file %s is empty", f)
		}
		if !isWithin(dest, f) {
			t.Errorf("file %s escaped dest %s", f, dest)
		}
	}

	// Directory structure and unicode names survived.
	for _, rel := range []string{
		"sub/dir1/file1.txt",
		"sub/dir2/file2.txt",
		"sub/with space/long fn.txt",
		"sub/üȵĩöḋè/file.txt",
	} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
			t.Errorf("expected member %s missing: %v", rel, err)
		}
	}
}

// TestExtractNative_RARSymlinksRefused is the end-to-end counterpart to the
// safeWriter unit test: a REAL archive carrying symlink members.
//
// unrar extracts data_link as a link and silently drops random_link (measured:
// "Dangerous link path"). We refuse both and still deliver the regular file —
// a stricter policy, verified against real archive bytes rather than a mode
// constant invented by the test.
func TestExtractNative_RARSymlinksRefused(t *testing.T) {
	dest := t.TempDir()
	files, err := extractNative(fixture(t, "rar5-symlink-unix.rar"), dest, "")
	if err != nil {
		t.Fatalf("extractNative: %v", err)
	}

	// The regular member came through — refusing links must not cost the payload.
	if len(files) != 1 || filepath.Base(files[0]) != "data.txt" {
		t.Errorf("files = %v, want just data.txt", files)
	}

	// No symlink exists anywhere under dest.
	err = filepath.Walk(dest, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Errorf("symlink was created at %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	for _, name := range []string{"data_link", "random_link"} {
		if _, err := os.Lstat(filepath.Join(dest, name)); !os.IsNotExist(err) {
			t.Errorf("%s exists, want refused", name)
		}
	}
}

// TestExtractNative_RARPassword covers encrypted bodies, with the wrong-password
// counterfactual that proves the success case is not vacuous.
func TestExtractNative_RARPassword(t *testing.T) {
	archive := fixture(t, "rar5-psw.rar")

	t.Run("correct password", func(t *testing.T) {
		dest := t.TempDir()
		files, err := extractNative(archive, dest, "password")
		if err != nil {
			t.Fatalf("extractNative: %v", err)
		}
		if len(files) == 0 {
			t.Fatal("no files extracted")
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			if len(b) == 0 {
				t.Errorf("%s is empty", f)
			}
		}
	})

	// COUNTERFACTUAL: a wrong password FAILS LOUDLY. This is the assertion that
	// matters — a decoder that returned garbage bytes as success would corrupt a
	// library silently, which is far worse than refusing to extract.
	t.Run("wrong password", func(t *testing.T) {
		dest := t.TempDir()
		_, err := extractNative(archive, dest, "definitely-not-it")
		if err == nil {
			t.Fatal("wrong password succeeded, want an error")
		}
		var pwErr *PasswordError
		if !errors.As(err, &pwErr) {
			t.Errorf("error = %v, want *PasswordError", err)
		}
	})

	// No password at all is also a failure, not a silent empty result.
	t.Run("no password", func(t *testing.T) {
		dest := t.TempDir()
		if _, err := extractNative(archive, dest, ""); err == nil {
			t.Error("missing password succeeded, want an error")
		}
	})
}

// TestIsNativePasswordProtected_RAR pins the pre-flight check that the pipeline
// uses to fail before starting a doomed extraction.
func TestIsNativePasswordProtected_RAR(t *testing.T) {
	if !isNativePasswordProtected(fixture(t, "rar5-psw.rar")) {
		t.Error("encrypted archive reported as unprotected")
	}
	// COUNTERFACTUAL: a plain archive is NOT reported as protected, so the check
	// above is not just returning true for everything.
	if isNativePasswordProtected(fixture(t, "rar5-subdirs.rar")) {
		t.Error("plain archive reported as password protected")
	}
}

// makeZip builds a zip in memory. Members are written verbatim, so a hostile
// name reaches the extractor exactly as a malicious archive would deliver it.
func makeZip(t *testing.T, path string, members map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range members {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write zip: %v", err)
	}
}

// TestExtractNative_ZIP covers the format that used to be invisible: a
// single-volume .zip release was never even detected as an archive, so it went
// unextracted regardless of which binaries were installed.
func TestExtractNative_ZIP(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "release.zip")
	makeZip(t, archive, map[string]string{
		"movie.mkv":    "video-bytes",
		"subs/eng.srt": "subtitle-bytes",
	})

	dest := t.TempDir()
	files, err := extractNative(archive, dest, "")
	if err != nil {
		t.Fatalf("extractNative: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("extracted %d files, want 2: %v", len(files), files)
	}
	got, err := os.ReadFile(filepath.Join(dest, "movie.mkv"))
	if err != nil {
		t.Fatalf("read movie.mkv: %v", err)
	}
	if string(got) != "video-bytes" {
		t.Errorf("content = %q, want %q", got, "video-bytes")
	}
}

// TestExtractNative_ZipSlip is the explicit zip-slip case, end to end through a
// real archive rather than through safeWriter directly.
func TestExtractNative_ZipSlip(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.zip")
	makeZip(t, archive, map[string]string{
		"../../../etc/passwd": "root::0:0:pwned",
		"movie.mkv":           "video-bytes",
	})

	dest := t.TempDir()
	// A sentinel where the traversal would land if it worked.
	victim := filepath.Join(filepath.Dir(dest), "etc", "passwd")

	files, err := extractNative(archive, dest, "")
	if err != nil {
		t.Fatalf("a hostile member must not abort the release: %v", err)
	}

	// The legitimate file still came through.
	if len(files) != 1 || filepath.Base(files[0]) != "movie.mkv" {
		t.Errorf("files = %v, want just movie.mkv", files)
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Errorf("traversal wrote outside dest at %s", victim)
	}
	// And nothing at all escaped, wherever it might have aimed.
	if _, err := os.Stat(filepath.Join(dest, "..", "etc", "passwd")); !os.IsNotExist(err) {
		t.Error("a file was created outside the destination")
	}
}

// TestDetectFormat pins format sniffing, including the case the extension
// cannot answer: a .001 volume whose container is only knowable from its bytes.
func TestDetectFormat(t *testing.T) {
	dir := t.TempDir()

	zipPath := filepath.Join(dir, "a.zip")
	makeZip(t, zipPath, map[string]string{"f.txt": "x"})

	// A zip named like a split volume — extension says nothing, bytes say zip.
	splitPath := filepath.Join(dir, "a.001")
	zipBytes, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	if err := os.WriteFile(splitPath, zipBytes, 0o644); err != nil {
		t.Fatalf("write split: %v", err)
	}

	// Raw data that merely LOOKS like a volume. Must not be claimed as an
	// archive: treating it as one makes the caller delete the originals.
	rawPath := filepath.Join(dir, "video.001")
	if err := os.WriteFile(rawPath, []byte("just raw video bytes, no header"), 0o644); err != nil {
		t.Fatalf("write raw: %v", err)
	}

	tests := []struct {
		path string
		want archiveFormat
	}{
		{fixture(t, "rar5-subdirs.rar"), formatRAR},
		{zipPath, formatZIP},
		{splitPath, formatZIP},
		{rawPath, formatUnknown},
		{filepath.Join(dir, "does-not-exist"), formatUnknown},
	}
	for _, tt := range tests {
		if got := detectFormat(tt.path); got != tt.want {
			t.Errorf("detectFormat(%s) = %v, want %v", filepath.Base(tt.path), got, tt.want)
		}
	}
}

// TestExtractNative_UnknownFormat pins the signal Extract relies on to decide
// whether the shell fallback is worth trying.
func TestExtractNative_UnknownFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mystery.rar")
	if err := os.WriteFile(path, []byte("not an archive at all"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := extractNative(path, t.TempDir(), "")
	if !errors.Is(err, errUnsupportedFormat) {
		t.Errorf("error = %v, want errUnsupportedFormat", err)
	}
}

// TestWrapPasswordErr covers the message-matching path, which exists because
// sevenzip reports a wrong password as an LZMA decode failure with no typed
// error to match on.
func TestWrapPasswordErr(t *testing.T) {
	var pwErr *PasswordError

	if err := wrapPasswordErr("a.rar", errors.New("rardecode: incorrect password")); !errors.As(err, &pwErr) {
		t.Errorf("incorrect-password message not mapped: %v", err)
	}
	if err := wrapPasswordErr("a.7z", errors.New("Wrong password detected")); !errors.As(err, &pwErr) {
		t.Errorf("wrong-password message not mapped: %v", err)
	}

	// COUNTERFACTUAL: an unrelated failure is passed through untouched. Mapping
	// it to PasswordError would make Extract skip the shell fallback (which it
	// deliberately does for password errors) and lose the rescue.
	other := errors.New("unexpected EOF")
	if err := wrapPasswordErr("a.rar", other); errors.As(err, &pwErr) {
		t.Errorf("unrelated error mapped to PasswordError: %v", err)
	} else if !errors.Is(err, other) {
		t.Errorf("unrelated error not passed through: %v", err)
	}

	if wrapPasswordErr("a.rar", nil) != nil {
		t.Error("nil error must stay nil")
	}
}

// TestSevenzipHeaderEncrypted covers the signal that a header-encrypted 7z
// produces, and the uncertainty it carries.
//
// REGRESSION: this case was missed on the first pass. A -mhe=on archive makes
// sevenzip fail with "unexpected id" — no mention of a password anywhere — so
// it was reported as a generic decode failure and the pipeline never asked for
// a password. Caught by an existing engine test whose fixture used -mhe=on.
func TestSevenzipHeaderEncrypted(t *testing.T) {
	encrypted := errors.New("sevenzip: error initialising: sevenzip: read error: sevenzip: unexpected id")
	if !sevenzipHeaderEncrypted(encrypted) {
		t.Error("header-encrypted signal not recognised")
	}

	// COUNTERFACTUAL: unrelated sevenzip failures are NOT claimed as password
	// problems. Without this the matcher could return true for everything and
	// every corrupt archive would be reported as encrypted.
	for _, msg := range []string{
		"sevenzip: read error: lzma: unsupported chunk header byte",
		"unexpected EOF",
		"",
	} {
		if msg == "" {
			if sevenzipHeaderEncrypted(nil) {
				t.Error("nil error treated as header-encrypted")
			}
			continue
		}
		if sevenzipHeaderEncrypted(errors.New(msg)) {
			t.Errorf("unrelated error treated as header-encrypted: %q", msg)
		}
	}

	// The verdict is marked uncertain, which is what lets Extract still try the
	// shell fallback (7z CAN tell encrypted from corrupt).
	err := wrapPasswordErr("a.7z", encrypted)
	var pwErr *PasswordError
	if !errors.As(err, &pwErr) {
		t.Fatalf("error = %v, want *PasswordError", err)
	}
	if !pwErr.Uncertain {
		t.Error("header-encrypted verdict not marked uncertain")
	}
	if !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("uncertain verdict does not admit the ambiguity: %q", err)
	}

	// COUNTERFACTUAL: a REPORTED password error is certain, so Extract skips the
	// pointless retry. If both were uncertain the distinction would be dead code.
	certain := wrapPasswordErr("a.rar", errors.New("rardecode: incorrect password"))
	if !errors.As(certain, &pwErr) {
		t.Fatalf("error = %v, want *PasswordError", certain)
	}
	if pwErr.Uncertain {
		t.Error("a reported password error was marked uncertain")
	}
}

// TestZipEncrypted pins the flag read, since archive/zip exposes no accessor.
func TestZipEncrypted(t *testing.T) {
	if !zipEncrypted(0x1) {
		t.Error("encrypted flag not detected")
	}
	if !zipEncrypted(0x9) { // encryption + data descriptor
		t.Error("encrypted flag not detected alongside other flags")
	}
	if zipEncrypted(0x8) { // data descriptor only
		t.Error("unencrypted member reported as encrypted")
	}
}

// make7z builds a 7z fixture with the 7z binary, skipping when it is absent.
// Used only where the format itself is under test and no library can write it.
func make7z(t *testing.T, args ...string) {
	t.Helper()
	sz, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z not installed; cannot build a 7z fixture")
	}
	cmd := exec.Command(sz, append([]string{"a"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not build 7z fixture: %v\n%s", err, out)
	}
}

// TestExtractNative_SevenZip covers a single-volume .7z — a format that used to
// be unextractable regardless of installed binaries, because nothing recognised
// it as an archive in the first place.
func TestExtractNative_SevenZip(t *testing.T) {
	dir := t.TempDir()
	payload := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(payload, []byte("video-bytes"), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	archive := filepath.Join(dir, "release.7z")
	make7z(t, "-t7z", archive, payload)

	dest := t.TempDir()
	files, err := extractNative(archive, dest, "")
	if err != nil {
		t.Fatalf("extractNative: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("extracted %d files, want 1: %v", len(files), files)
	}
	got, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "video-bytes" {
		t.Errorf("content = %q, want %q", got, "video-bytes")
	}
}

// TestExtractNative_SevenZipHeaderEncrypted is the end-to-end form of the
// regression: a -mhe=on archive must be reported as needing a password, not as
// an opaque decode failure.
func TestExtractNative_SevenZipHeaderEncrypted(t *testing.T) {
	dir := t.TempDir()
	payload := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(payload, []byte("video-bytes"), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	archive := filepath.Join(dir, "secret.7z")
	make7z(t, "-t7z", "-psecret", "-mhe=on", archive, payload)

	// Without the password: reported as a password problem.
	_, err := extractNative(archive, t.TempDir(), "")
	var pwErr *PasswordError
	if !errors.As(err, &pwErr) {
		t.Errorf("error = %v, want *PasswordError", err)
	}
	if !isNativePasswordProtected(archive) {
		t.Error("header-encrypted archive not detected as password protected")
	}

	// COUNTERFACTUAL: with the password it extracts cleanly, so the detection
	// above is not just reporting a broken archive.
	dest := t.TempDir()
	files, err := extractNative(archive, dest, "secret")
	if err != nil {
		t.Fatalf("extraction with the correct password failed: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("extracted %d files, want 1", len(files))
	}
	got, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "video-bytes" {
		t.Errorf("content = %q, want %q", got, "video-bytes")
	}
}

// TestExtractNative_SevenZipMultiVolume covers .7z.001 sets, which the earlier
// analysis had never tested — the assumption was that only RAR multi-volume
// worked natively.
func TestExtractNative_SevenZipMultiVolume(t *testing.T) {
	dir := t.TempDir()
	payload := filepath.Join(dir, "movie.mkv")
	// Large enough to actually span volumes at -v64k.
	if err := os.WriteFile(payload, bytes.Repeat([]byte("A"), 300_000), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	archive := filepath.Join(dir, "release.7z")
	make7z(t, "-t7z", "-mx0", "-v64k", archive, payload)

	first := archive + ".001"
	if _, err := os.Stat(first); err != nil {
		t.Skipf("7z did not split the fixture: %v", err)
	}
	// PREMISE: it really is a multi-volume set, else this tests nothing.
	if _, err := os.Stat(archive + ".002"); err != nil {
		t.Skipf("fixture has only one volume: %v", err)
	}

	dest := t.TempDir()
	files, err := extractNative(first, dest, "")
	if err != nil {
		t.Fatalf("extractNative on a split set: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("extracted %d files, want 1: %v", len(files), files)
	}
	st, err := os.Stat(files[0])
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() != 300_000 {
		t.Errorf("size = %d, want 300000 — volumes were not joined", st.Size())
	}
}

// TestExtractNative_SkippedMembersAreReported guards the property that makes a
// refusal debuggable: a release that quietly loses half its files must be
// distinguishable from a clean one.
func TestExtractNative_SkippedMembersAreReported(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.zip")
	makeZip(t, archive, map[string]string{
		"../escape.txt": "x",
		"good.mkv":      "y",
	})

	w, err := newSafeWriter(t.TempDir())
	if err != nil {
		t.Fatalf("newSafeWriter: %v", err)
	}
	files, err := extractZIPNative(archive, "", w)
	if err != nil {
		t.Fatalf("extractZIPNative: %v", err)
	}
	if len(files) != 1 {
		t.Errorf("files = %v, want 1", files)
	}
	if len(w.skipped) != 1 || !strings.Contains(w.skipped[0], "escape.txt") {
		t.Errorf("skipped = %v, want the refused member recorded", w.skipped)
	}
}
