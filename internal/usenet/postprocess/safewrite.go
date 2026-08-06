package postprocess

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The native extractors hand us member names and link targets exactly as they
// appear inside the archive — an attacker-controlled string. Shelling out to
// unrar/7z used to hide that: both refuse to write outside the destination and
// print "Dangerous link path was ignored" for an escaping symlink. Decoding in
// Go moves that decision here, so every path a native extractor writes goes
// through safeWriter and nothing else creates files.

// maxSymlinkModeBits are the file-mode bits that mark a member as something
// other than a plain file or directory. Anything carrying them is refused: a
// media release has no legitimate use for a device node, a socket or a setuid
// bit, and honouring them only widens what a hostile archive can do.
const disallowedModes = os.ModeSymlink | os.ModeDevice | os.ModeNamedPipe |
	os.ModeSocket | os.ModeCharDevice | os.ModeIrregular | os.ModeSetuid | os.ModeSetgid

// safeWriter materialises archive members under a fixed destination directory,
// refusing anything that would write outside it.
type safeWriter struct {
	destDir string

	// skipped counts members refused for safety. Surfaced so a release that
	// silently loses half its files is distinguishable from a clean one.
	skipped []string
}

func newSafeWriter(destDir string) (*safeWriter, error) {
	abs, err := filepath.Abs(destDir)
	if err != nil {
		return nil, fmt.Errorf("resolve dest dir: %w", err)
	}
	// The destination is resolved through symlinks ONCE, here. Every later
	// containment check compares against this resolved base, so a download dir
	// that merely sits behind a symlink (macOS /var → /private/var) is treated
	// as mount indirection rather than as an escape.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return &safeWriter{destDir: abs}, nil
}

// resolve maps an archive member name to an absolute path under destDir.
//
// Rejects absolute names, drive-relative Windows names and any traversal that
// climbs out — including the case where a leading component is itself an
// existing symlink pointing elsewhere.
func (w *safeWriter) resolve(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty member name")
	}

	// Archives store forward slashes regardless of the writing platform, so
	// normalise before any path reasoning. Done first: on Windows a "\" would
	// otherwise survive as a literal character inside a single component and
	// slip past the traversal check below.
	clean := strings.ReplaceAll(name, "\\", "/")

	if strings.HasPrefix(clean, "/") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("absolute path: %q", name)
	}
	// "C:foo" / "C:/foo" — drive-relative on Windows, harmless-looking elsewhere.
	if len(clean) >= 2 && clean[1] == ':' {
		return "", fmt.Errorf("drive-relative path: %q", name)
	}

	joined := filepath.Join(w.destDir, filepath.FromSlash(clean))
	if !isWithin(w.destDir, joined) {
		return "", fmt.Errorf("path escapes destination: %q", name)
	}

	// filepath.Join collapses "..", so the check above catches a name that
	// spells out its escape. It does NOT catch an escape through a symlink that
	// an EARLIER member of this same archive planted — the classic two-step:
	// member 1 is "link" → "../..", member 2 is "link/payload". Symlink members
	// are refused outright (see checkMode), which removes that vector at the
	// source; verifying the resolved parent as well keeps the guarantee even if
	// a link was already sitting in the destination before extraction started.
	parent := filepath.Dir(joined)
	if real, err := filepath.EvalSymlinks(parent); err == nil {
		if !isWithin(w.destDir, real) {
			return "", fmt.Errorf("parent directory escapes destination via symlink: %q", name)
		}
	}

	return joined, nil
}

// checkMode refuses members that are not plain files or directories.
//
// Symlinks are rejected unconditionally, which is stricter than unrar (it
// creates links whose target stays inside). A video release has no use for
// them, and allowing them means re-deriving, per member, whether a target is
// safe — the exact reasoning that CVE after CVE has got wrong. Refusing is a
// property of the writer, not a judgement call at each call site.
func checkMode(mode os.FileMode) error {
	if mode&disallowedModes != 0 {
		return fmt.Errorf("unsupported file type (%s)", mode.Type())
	}
	return nil
}

// writeFile streams r into the member named name.
//
// Returns the written path. A member refused for safety is recorded in skipped
// and returns ("", nil) — a hostile entry must not abort a release whose other
// files are perfectly good, but it must not pass unnoticed either.
func (w *safeWriter) writeFile(name string, mode os.FileMode, r io.Reader) (string, error) {
	if err := checkMode(mode); err != nil {
		w.skip(name, err)
		return "", nil
	}
	path, err := w.resolve(name)
	if err != nil {
		w.skip(name, err)
		return "", nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create dir for %q: %w", name, err)
	}

	// noFollowFlag: refuse to write THROUGH an existing symlink sitting at the
	// destination path. Without it, a link planted by any earlier process (or a
	// previous run) turns a write inside destDir into a write wherever it points.
	// O_TRUNC rather than O_EXCL so re-extracting over a previous attempt still
	// works, which is what the shell extractors do with -o+/-y.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|noFollowFlag, filePerm(mode))
	if err != nil {
		return "", fmt.Errorf("create %q: %w", name, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, r); err != nil {
		return "", fmt.Errorf("write %q: %w", name, err)
	}
	return path, nil
}

// mkdir creates a directory member.
func (w *safeWriter) mkdir(name string) error {
	path, err := w.resolve(name)
	if err != nil {
		w.skip(name, err)
		return nil
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create dir %q: %w", name, err)
	}
	return nil
}

func (w *safeWriter) skip(name string, reason error) {
	w.skipped = append(w.skipped, fmt.Sprintf("%s (%v)", name, reason))
}

// filePerm keeps the archive's user-executable bit but never more than 0755,
// and never the setuid/setgid bits (already refused by checkMode — belt and
// braces, since permissions are applied by the OS without further review).
func filePerm(mode os.FileMode) os.FileMode {
	perm := mode.Perm() & 0o755
	if perm == 0 {
		return 0o644
	}
	// Ensure the owner can always read back what was just written; some archives
	// carry 0000 for members that the writing tool never intended to be private.
	return perm | 0o600
}

// isWithin reports whether target is base or lives under it.
//
// Deliberately NOT a copy of engine.isWithinDir: importing engine here would
// invert the dependency (engine already imports postprocess). Same semantics,
// stated once for this package.
func isWithin(base, target string) bool {
	base = filepath.Clean(base)
	target = filepath.Clean(target)
	return target == base || strings.HasPrefix(target, base+string(filepath.Separator))
}
