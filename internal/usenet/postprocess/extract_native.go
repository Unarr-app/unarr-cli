package postprocess

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/bodgit/sevenzip"
	"github.com/nwaples/rardecode/v2"
)

// Native, in-process extraction — no external binary required.
//
// The shell extractors remain as a fallback (see Extract), but they are no
// longer a PREREQUISITE: a bare `unarr` binary on a machine with neither unrar
// nor 7z installed can now unpack a release on its own.
//
// Format is chosen by magic bytes, never by extension. That is what closes the
// old gap where a single-volume .7z or .zip release went unextracted because
// isArchiveFile only ever recognised .rar/.rNN/.001.

// archiveFormat identifies the container inside a file.
type archiveFormat int

const (
	formatUnknown archiveFormat = iota
	formatRAR
	format7z
	formatZIP
)

func (f archiveFormat) String() string {
	switch f {
	case formatRAR:
		return "rar"
	case format7z:
		return "7z"
	case formatZIP:
		return "zip"
	default:
		return "unknown"
	}
}

// detectFormat sniffs the container from the file's leading bytes.
//
// A .001 volume of a split set carries the header of whatever container it
// splits, which is exactly why this is content-sniffed: the extension says
// nothing.
func detectFormat(path string) archiveFormat {
	f, err := os.Open(path)
	if err != nil {
		return formatUnknown
	}
	defer f.Close()

	buf := make([]byte, 8)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return formatUnknown
	}
	head := string(buf[:n])

	switch {
	case strings.HasPrefix(head, "Rar!\x1a\x07"): // RAR4 and RAR5
		return formatRAR
	case n >= 6 && strings.HasPrefix(head, "7z\xbc\xaf\x27\x1c"):
		return format7z
	case strings.HasPrefix(head, "PK\x03\x04"),
		strings.HasPrefix(head, "PK\x05\x06"),
		strings.HasPrefix(head, "PK\x07\x08"):
		return formatZIP
	}
	return formatUnknown
}

// errUnsupportedFormat means the native path does not recognise the container.
// Extract treats it as a reason to try the shell extractors, not as a failure.
var errUnsupportedFormat = errors.New("native extractor: unrecognised archive format")

// extractNative unpacks archivePath into outputDir without spawning a process.
//
// Multi-volume sets are followed automatically by both libraries from the entry
// volume — the caller passes .part01.rar / .7z.001 and the rest is found on disk.
func extractNative(archivePath, outputDir, password string) ([]string, error) {
	w, err := newSafeWriter(outputDir)
	if err != nil {
		return nil, err
	}

	var files []string
	format := detectFormat(archivePath)
	switch format {
	case formatRAR:
		files, err = extractRARNative(archivePath, password, w)
	case format7z:
		files, err = extract7zNative(archivePath, password, w)
	case formatZIP:
		files, err = extractZIPNative(archivePath, password, w)
	case formatUnknown:
		return nil, errUnsupportedFormat
	default:
		return nil, errUnsupportedFormat
	}
	if err != nil {
		return nil, err
	}

	for _, s := range w.skipped {
		log.Printf("[extract] refused unsafe archive member: %s", s)
	}
	return files, nil
}

// extractRARNative handles RAR3/RAR5, solid, multi-volume and encrypted
// (including header-encrypted) archives via rardecode.
func extractRARNative(archivePath, password string, w *safeWriter) ([]string, error) {
	var opts []rardecode.Option
	if password != "" {
		opts = append(opts, rardecode.Password(password))
	}

	rc, err := rardecode.OpenReader(archivePath, opts...)
	if err != nil {
		return nil, wrapPasswordErr(archivePath, err)
	}
	defer rc.Close()

	var files []string
	for {
		h, err := rc.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, wrapPasswordErr(archivePath, fmt.Errorf("rar: %w", err))
		}

		if h.IsDir {
			if err := w.mkdir(h.Name); err != nil {
				return nil, err
			}
			continue
		}

		path, err := w.writeFile(h.Name, h.Mode(), rc)
		if err != nil {
			return nil, wrapPasswordErr(archivePath, err)
		}
		if path != "" {
			files = append(files, path)
		}
	}
	return files, nil
}

// extract7zNative handles 7z, including multi-volume (.7z.001) and encrypted
// archives.
func extract7zNative(archivePath, password string, w *safeWriter) ([]string, error) {
	var (
		rc  *sevenzip.ReadCloser
		err error
	)
	if password != "" {
		rc, err = sevenzip.OpenReaderWithPassword(archivePath, password)
	} else {
		rc, err = sevenzip.OpenReader(archivePath)
	}
	if err != nil {
		return nil, wrapPasswordErr(archivePath, err)
	}
	defer rc.Close()

	var files []string
	for _, f := range rc.File {
		info := f.FileInfo()
		if info.IsDir() {
			if err := w.mkdir(f.Name); err != nil {
				return nil, err
			}
			continue
		}

		// Refuse before opening: a symlink member's "content" is its target,
		// and there is no reason to decode bytes we will not write.
		if err := checkMode(info.Mode()); err != nil {
			w.skip(f.Name, err)
			continue
		}

		r, err := f.Open()
		if err != nil {
			return nil, wrapPasswordErr(archivePath, fmt.Errorf("7z: open %q: %w", f.Name, err))
		}
		path, err := w.writeFile(f.Name, info.Mode(), r)
		r.Close()
		if err != nil {
			return nil, wrapPasswordErr(archivePath, err)
		}
		if path != "" {
			files = append(files, path)
		}
	}
	return files, nil
}

// extractZIPNative handles single-volume zip via the standard library.
//
// Encrypted zip is NOT supported: stdlib has no decryption, and the formats in
// the wild (ZipCrypto, WinZip AES) would each need a hand-rolled implementation.
// An encrypted member surfaces as a normal read error and Extract falls back to
// 7z, which does handle it.
func extractZIPNative(archivePath string, _ string, w *safeWriter) ([]string, error) {
	rc, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("zip: %w", err)
	}
	defer rc.Close()

	var files []string
	for _, f := range rc.File {
		info := f.FileInfo()
		if info.IsDir() {
			if err := w.mkdir(f.Name); err != nil {
				return nil, err
			}
			continue
		}
		if err := checkMode(info.Mode()); err != nil {
			w.skip(f.Name, err)
			continue
		}

		r, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("zip: open %q: %w", f.Name, err)
		}
		path, err := w.writeFile(f.Name, info.Mode(), r)
		r.Close()
		if err != nil {
			return nil, err
		}
		if path != "" {
			files = append(files, path)
		}
	}
	return files, nil
}

// wrapPasswordErr converts a library-specific "bad password" signal into the
// *PasswordError the pipeline already understands, so the native path is
// indistinguishable from the shell path at the call site.
//
// Matching is by sentinel where the library offers one (rardecode.ErrBadPassword)
// and by message otherwise: sevenzip surfaces a wrong password as a decoder
// failure deep inside LZMA ("unsupported chunk header byte"), because decrypting
// with the wrong key yields bytes that are not valid compressed data. That is
// noisy-but-correct behaviour — it never returns garbage as success — yet it
// carries no typed error to match on.
func wrapPasswordErr(archivePath string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, rardecode.ErrBadPassword) {
		return &PasswordError{Archive: archivePath}
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "incorrect password") ||
		strings.Contains(msg, "wrong password") ||
		strings.Contains(msg, "password required") {
		return &PasswordError{Archive: archivePath}
	}
	if sevenzipHeaderEncrypted(err) {
		return &PasswordError{Archive: archivePath, Uncertain: true}
	}
	return err
}

// sevenzipHeaderEncrypted reports whether a sevenzip failure looks like a
// header-encrypted archive (-mhe=on) that was opened without a usable password.
//
// AMBIGUOUS BY NATURE: an encrypted header and a CORRUPT header fail the same
// way — the metadata will not parse and sevenzip says "unexpected id" for both.
// Nothing in the error distinguishes them, and re-opening the file cannot
// either, since the second attempt hits the identical wall.
//
// Reporting it as a password problem is the deliberate choice. The two outcomes
// are not symmetric: telling a user with an encrypted archive that it is corrupt
// sends them to delete a perfectly good download, whereas telling a user with a
// corrupt archive that it may need a password costs them one failed attempt
// before they look further. Extract also tries the shell fallback first
// whenever a binary is installed, and 7z DOES tell the two apart — so this
// verdict is only ever the final word on a machine with no 7z at all.
func sevenzipHeaderEncrypted(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sevenzip:") && strings.Contains(msg, "unexpected id")
}

// zipEncrypted reports whether a zip member is encrypted.
//
// Bit 0 of the general-purpose flag field (APPNOTE 4.4.4). archive/zip exposes
// the raw Flags but offers no accessor, and its Open() returns a plain
// ErrAlgorithm for encrypted members — indistinguishable from an unsupported
// compression method, so the flag is read directly.
func zipEncrypted(flags uint16) bool {
	return flags&0x1 != 0
}

// isNativePasswordProtected reports whether the archive needs a password,
// without needing an extractor binary.
//
// Only the header is inspected — no member is decoded — so this stays cheap
// even for a multi-gigabyte set.
func isNativePasswordProtected(archivePath string) bool {
	switch detectFormat(archivePath) {
	case formatRAR:
		// A header-encrypted RAR cannot even be opened without the password;
		// a body-encrypted one opens but reports encryption per member.
		rc, err := rardecode.OpenReader(archivePath)
		if err != nil {
			return errors.Is(err, rardecode.ErrBadPassword)
		}
		defer rc.Close()
		for {
			h, err := rc.Next()
			if errors.Is(err, io.EOF) {
				return false
			}
			if err != nil {
				return errors.Is(err, rardecode.ErrBadPassword)
			}
			if h.Encrypted {
				return true
			}
		}
	case format7z:
		rc, err := sevenzip.OpenReader(archivePath)
		if err != nil {
			// A header-encrypted 7z (-mhe=on) hides its own file list, so
			// sevenzip cannot even parse the metadata and fails with
			// "unexpected id" — no mention of a password anywhere. Measured
			// against a real -mhe fixture; the same archive opens cleanly once
			// the password is supplied, and a plain 7z never produces this.
			return sevenzipHeaderEncrypted(err)
		}
		defer rc.Close()
		for _, f := range rc.File {
			if f.FileInfo().IsDir() {
				continue
			}
			// An encrypted 7z lists its members fine; the failure appears on
			// first read. Opening alone does not decode, so probe one member.
			r, err := f.Open()
			if err != nil {
				return true
			}
			_, err = io.CopyN(io.Discard, r, 1)
			r.Close()
			if err != nil && !errors.Is(err, io.EOF) {
				return true
			}
			return false
		}
		return false
	case formatZIP:
		rc, err := zip.OpenReader(archivePath)
		if err != nil {
			return false
		}
		defer rc.Close()
		for _, f := range rc.File {
			if zipEncrypted(f.Flags) {
				return true
			}
		}
		return false
	case formatUnknown:
		return false
	}
	return false
}
