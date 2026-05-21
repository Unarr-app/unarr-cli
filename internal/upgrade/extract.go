package upgrade

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// extractBinary extracts the unarr binary from the release archive into destDir.
// Returns the path to the extracted binary.
func extractBinary(archivePath, destDir string) (string, error) {
	if runtime.GOOS == "windows" {
		return extractZip(archivePath, destDir)
	}
	return extractTarGz(archivePath, destDir)
}

func extractTarGz(archivePath, destDir string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	target := binaryName
	if runtime.GOOS == "windows" {
		target += ".exe"
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar: %w", err)
		}

		name := filepath.Base(hdr.Name)
		if name != target {
			continue
		}

		// Validate: must be a regular file
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		dst := filepath.Join(destDir, target)
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return "", err
		}

		if _, err := io.Copy(out, io.LimitReader(tr, 200<<20)); err != nil { // 200MB limit
			out.Close()
			return "", fmt.Errorf("extract: %w", err)
		}
		out.Close()
		return dst, nil
	}

	return "", fmt.Errorf("binary %q not found in archive", target)
}

func extractZip(archivePath, destDir string) (string, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", fmt.Errorf("zip: %w", err)
	}
	defer r.Close()

	target := binaryName + ".exe"

	// Resolve destDir to its absolute form once so the ZIP-slip check below
	// can compare canonical paths instead of fragile substring matches.
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return "", fmt.Errorf("resolve dest: %w", err)
	}

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if filepath.Base(f.Name) != target {
			continue
		}
		absDst, ok := safeZipPath(f.Name, target, absDest)
		if !ok {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return "", err
		}

		out, err := os.OpenFile(absDst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			rc.Close()
			return "", err
		}

		if _, err := io.Copy(out, io.LimitReader(rc, 200<<20)); err != nil { // 200MB limit
			out.Close()
			rc.Close()
			return "", fmt.Errorf("extract: %w", err)
		}
		out.Close()
		rc.Close()
		return absDst, nil
	}

	return "", fmt.Errorf("binary %q not found in archive", target)
}

// safeZipPath validates that a ZIP entry name is safe to extract under
// absDest, then returns the absolute destination path (always
// absDest/target, never the raw entry name — we still only extract files
// matched by Base name).
//
// Rejected: absolute paths, paths that resolve to "..", paths containing
// a "../" or "..\\" component, and any entry whose final destination
// would land outside absDest. The check uses path.Clean on the entry's
// native separator (ZIP uses forward slashes by spec, but some authors
// emit backslashes — we treat both as separators here so a hostile entry
// on Linux can't bypass the substring scan).
func safeZipPath(entryName, target, absDest string) (string, bool) {
	// Normalise both separators to "/" so the check works on Linux too,
	// where filepath.Separator is "/" and a hostile "..\\foo" string is
	// otherwise treated as a single filename component by filepath.Clean.
	normalised := strings.ReplaceAll(entryName, `\`, "/")
	cleaned := path.Clean(normalised)
	if cleaned == ".." ||
		strings.HasPrefix(cleaned, "../") ||
		strings.Contains(cleaned, "/../") ||
		path.IsAbs(cleaned) {
		return "", false
	}
	absDst, err := filepath.Abs(filepath.Join(absDest, target))
	if err != nil {
		return "", false
	}
	if !strings.HasPrefix(absDst+string(filepath.Separator), absDest+string(filepath.Separator)) {
		return "", false
	}
	return absDst, true
}
