package backup

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// Size bounds, checked before anything is read into memory. Real archives are a
// few KB; config.toml and the queue never approach these. maxMemberSize is the
// zip-bomb guard (decompressed), maxArchiveSize keeps a stray multi-GB file from
// being slurped, and maxMembers caps the member count (manifest + allowlist,
// with headroom for a future schema).
const (
	maxMemberSize  = 16 << 20
	maxArchiveSize = 64 << 20
	maxMembers     = 16
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

// readBounded reads the archive file, refusing one over maxArchiveSize.
func readBounded(file string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("read backup: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxArchiveSize+1))
	if err != nil {
		return nil, fmt.Errorf("read backup: %w", err)
	}
	if len(raw) > maxArchiveSize {
		return nil, fmt.Errorf("%w: %s is over %d MiB", ErrTooLarge, file, maxArchiveSize>>20)
	}
	return raw, nil
}

// readMembers loads every member, rejecting names that are not a bare file
// name: that is the zip-slip defence at the archive level (restore adds the
// allowlist on top).
func readMembers(zr *zip.Reader) (map[string][]byte, error) {
	if len(zr.File) > maxMembers {
		return nil, fmt.Errorf("%w: %d members", ErrTooLarge, len(zr.File))
	}
	out := make(map[string][]byte, len(zr.File))
	for _, f := range zr.File {
		if !safeMemberName(f.Name) {
			return nil, fmt.Errorf("%w: %q", ErrUnsafePath, f.Name)
		}
		if _, dup := out[f.Name]; dup {
			return nil, fmt.Errorf("%w: duplicate member %q", ErrBadManifest, f.Name)
		}
		if f.UncompressedSize64 > maxMemberSize {
			return nil, fmt.Errorf("%w: member %q", ErrTooLarge, f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("%w: %q: %v", ErrBadManifest, f.Name, err)
		}
		data, err := io.ReadAll(io.LimitReader(rc, maxMemberSize+1))
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%w: %q: %v", ErrChecksum, f.Name, err)
		}
		if len(data) > maxMemberSize {
			return nil, fmt.Errorf("%w: member %q", ErrTooLarge, f.Name)
		}
		out[f.Name] = data
	}
	return out, nil
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
