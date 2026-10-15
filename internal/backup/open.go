package backup

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
)

// Archive is a fully verified backup held in memory. Open only returns one
// when the manifest parsed, no member escapes the allowlist and every
// checksum matched, so everything downstream can trust it.
type Archive struct {
	Manifest  Manifest
	Encrypted bool
	files     map[string][]byte
}

// Open reads, decrypts (if needed) and verifies a backup file.
func Open(file, passphrase string) (*Archive, error) {
	raw, err := readBounded(file)
	if err != nil {
		return nil, err
	}
	enc := isEncrypted(raw)
	if enc {
		if raw, err = decrypt(raw, passphrase); err != nil {
			return nil, err
		}
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("%w: not a valid zip: %v", ErrBadManifest, err)
	}
	members, err := readMembers(zr)
	if err != nil {
		return nil, err
	}
	mb, ok := members[ManifestName]
	if !ok {
		return nil, fmt.Errorf("%w: %s missing", ErrBadManifest, ManifestName)
	}
	delete(members, ManifestName)
	m, err := parseManifest(mb)
	if err != nil {
		return nil, err
	}
	if err := verify(m, members); err != nil {
		return nil, err
	}
	return &Archive{Manifest: m, Encrypted: enc, files: members}, nil
}

// readBounded reads the archive file, refusing one over maxArchiveSize — by its
// size on disk before reading anything, and while reading if it grows.
func readBounded(file string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("read backup: %w", err)
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil {
		if err := checkArchiveSize(file, st.Size()); err != nil {
			return nil, err
		}
	}
	raw, over, err := readLimited(f, maxArchiveSize())
	if err != nil {
		return nil, fmt.Errorf("read backup: %w", err)
	}
	if over {
		return nil, checkArchiveSize(file, -1)
	}
	return raw, nil
}

// readMembers loads every member, rejecting names that are not a bare file
// name: that is the zip-slip defence at the archive level (restore adds the
// allowlist on top). Every member must be a regular file — a symlink's
// "content" is its target, which must never become a config — each must fit
// its own cap, and together they must fit maxContentSize.
func readMembers(zr *zip.Reader) (map[string][]byte, error) {
	if len(zr.File) > maxMembers {
		return nil, fmt.Errorf("%w: %d members", ErrTooLarge, len(zr.File))
	}
	out := make(map[string][]byte, len(zr.File))
	budget := newContentBudget()
	for _, f := range zr.File {
		if !safeMemberName(f.Name) {
			return nil, fmt.Errorf("%w: %q", ErrUnsafePath, f.Name)
		}
		if mode := f.Mode(); !mode.IsRegular() {
			return nil, fmt.Errorf("%w: %q is %s, not a regular file", ErrUnsafePath, f.Name, fileKind(mode))
		}
		if _, dup := out[f.Name]; dup {
			return nil, fmt.Errorf("%w: duplicate member %q", ErrBadManifest, f.Name)
		}
		data, err := readMember(f, budget)
		if err != nil {
			return nil, err
		}
		out[f.Name] = data
	}
	return out, nil
}

// fileKind words a non-regular member's type for the error.
func fileKind(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "a symlink"
	case mode.IsDir():
		return "a directory"
	default:
		return "a special file"
	}
}

// readMember inflates f within its room in budget (its own cap, or what is left
// of the shared bound): the size the zip declares is checked before inflating,
// then the bytes that actually come out, so a member that understates its size
// cannot get past the bound either.
func readMember(f *zip.File, budget *contentBudget) ([]byte, error) {
	room := budget.room(f.Name)
	if f.UncompressedSize64 > uint64(room) {
		return nil, budget.tooLarge(f.Name, -1)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %v", ErrBadManifest, f.Name, err)
	}
	data, over, err := readLimited(rc, room)
	_ = rc.Close()
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %v", ErrChecksum, f.Name, err)
	}
	if over {
		return nil, budget.tooLarge(f.Name, -1)
	}
	return data, budget.spend(f.Name, int64(len(data)))
}

// safeMemberName accepts only a plain file name: no directories, no "..",
// no backslashes or drive letters (Windows separators), no absolute paths.
func safeMemberName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\:`) || strings.ContainsRune(name, 0) {
		return false
	}
	return path.Base(name) == name
}

// verify checks that the members and the manifest describe exactly the same
// files, with matching size and SHA-256.
func verify(m Manifest, members map[string][]byte) error {
	seen := make(map[string]bool, len(m.Files))
	for _, e := range m.Files {
		if !safeMemberName(e.Name) {
			return fmt.Errorf("%w: %q", ErrUnsafePath, e.Name)
		}
		data, ok := members[e.Name]
		if !ok {
			return fmt.Errorf("%w: %q listed in the manifest but absent from the archive", ErrBadManifest, e.Name)
		}
		if int64(len(data)) != e.Size || sumHex(data) != strings.ToLower(e.SHA256) {
			return fmt.Errorf("%w: %s", ErrChecksum, e.Name)
		}
		seen[e.Name] = true
	}
	for name := range members {
		if !seen[name] {
			return fmt.Errorf("%w: %q is in the archive but not in the manifest", ErrBadManifest, name)
		}
	}
	return nil
}
