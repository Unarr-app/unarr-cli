package support

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// bundleFileMode is 0600 on the archive.
//
// The bundle is the most concentrated pile of diagnostics on the machine, and
// it is written into whatever directory the user happened to be standing in —
// often a shared /tmp or a synced folder. 0600 keeps a co-tenant from reading
// it before the user has looked at it themselves.
const bundleFileMode = 0o600

// entryFileMode is what the files INSIDE the archive unpack as. Same reasoning,
// carried across the extraction.
const entryFileMode = 0o600

// DefaultName is the archive name when the user gave no --out. The timestamp is
// UTC and sortable so a second bundle never silently overwrites the first —
// support threads routinely collect a "before" and an "after".
func DefaultName(now time.Time) string {
	return "unarr-support-" + now.UTC().Format("20060102-150405") + ".tar.gz"
}

// WriteTarGz writes the bundle to path.
//
// Sections that were not collected write no file — their absence and its reason
// live in manifest.json, which is written first so a reader who unpacks and
// looks at one file looks at the index.
//
// The file is created 0600 BEFORE anything is written to it (O_CREATE with the
// mode, not a chmod afterwards), so there is no window in which the archive
// exists world-readable.
func (b *Bundle) WriteTarGz(path string) error {
	manifest, err := b.Manifest()
	if err != nil {
		return fmt.Errorf("build manifest: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, bundleFileMode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := b.writeEntries(f, manifest); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// writeEntries streams the archive. Split from WriteTarGz so the file handling
// and the tar building are each one thing.
func (b *Bundle) writeEntries(f *os.File, manifest []byte) error {
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	root := strings.TrimSuffix(filepath.Base(f.Name()), ".tar.gz")

	if err := writeEntry(tw, root+"/manifest.json", manifest, b.GeneratedAt); err != nil {
		return err
	}
	for _, s := range b.Sections {
		if s.Absent != "" {
			continue
		}
		if err := writeEntry(tw, root+"/"+s.Name, s.body, b.GeneratedAt); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("finish archive: %w", err)
	}
	return gz.Close()
}

// writeEntry adds one file. Everything lives under a single top-level
// directory named after the archive, so unpacking in a home directory does not
// scatter a dozen loose files across it.
func writeEntry(tw *tar.Writer, name string, body []byte, mtime time.Time) error {
	h := &tar.Header{
		Name:     name,
		Mode:     entryFileMode,
		Size:     int64(len(body)),
		ModTime:  mtime,
		Typeflag: tar.TypeReg,
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(h); err != nil {
		return fmt.Errorf("write header %s: %w", name, err)
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

// Listing renders the human-readable table of what the bundle holds — what
// --print shows, and what the command prints after writing. An absent section
// is listed with its reason rather than hidden, because "no daemon log,
// because the file does not exist" is itself the answer often enough.
func (b *Bundle) Listing() string {
	var sb strings.Builder
	for _, s := range b.Sections {
		if s.Absent != "" {
			fmt.Fprintf(&sb, "  %-22s  —  %s\n", s.Name, s.Absent)
			continue
		}
		fmt.Fprintf(&sb, "  %-22s  %8d bytes", s.Name, s.Bytes)
		if s.Note != "" {
			fmt.Fprintf(&sb, "  (%s)", s.Note)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// Body returns a collected section's bytes, or nil when it was absent. Exists
// for --print and for tests; the archive writer reads the field directly.
func (b *Bundle) Body(name string) []byte {
	for _, s := range b.Sections {
		if s.Name == name {
			return s.body
		}
	}
	return nil
}
