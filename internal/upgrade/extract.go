package upgrade

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
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

	for _, f := range r.File {
		name := filepath.Base(f.Name)

		// Guard against path traversal
		if strings.Contains(f.Name, "..") {
			continue
		}

		if name != target {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return "", err
		}

		dst := filepath.Join(destDir, target)
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
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
		return dst, nil
	}

	return "", fmt.Errorf("binary %q not found in archive", target)
}
