package mediainfo

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// fpcalc (chromaprint) powers skip-segment detection: the ffmpeg static builds
// we download from ffbinaries do NOT include the chromaprint muxer, so audio
// fingerprinting pipes decoded WAV from our ffmpeg into a standalone fpcalc
// binary. acoustid publishes small (~2MB) static builds per platform.

const fpcalcVersion = "1.6.0"

var fpcalcDLClient = &http.Client{Timeout: 5 * time.Minute}

const maxFpcalcArchiveSize = 50 * 1024 * 1024 // 50MB

// fpcalcDownloadURL returns the release asset URL for the current platform,
// and whether the asset is a zip (Windows) instead of tar.gz.
func fpcalcDownloadURL() (url string, isZip bool, err error) {
	base := fmt.Sprintf("https://github.com/acoustid/chromaprint/releases/download/v%s/chromaprint-fpcalc-%s-", fpcalcVersion, fpcalcVersion)
	switch runtime.GOOS {
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			return base + "linux-x86_64.tar.gz", false, nil
		case "arm64":
			return base + "linux-arm64.tar.gz", false, nil
		}
	case "darwin":
		return base + "macos-universal.tar.gz", false, nil
	case "windows":
		if runtime.GOARCH == "amd64" {
			return base + "windows-x86_64.zip", true, nil
		}
	}
	return "", false, fmt.Errorf("no fpcalc build for platform %s/%s", runtime.GOOS, runtime.GOARCH)
}

// FpcalcCachePath returns the cached fpcalc binary path (same bin dir as the
// downloaded ffmpeg/ffprobe).
func FpcalcCachePath() (string, error) {
	dir, err := FFprobeCacheDir()
	if err != nil {
		return "", err
	}
	name := "fpcalc"
	if runtime.GOOS == "windows" {
		name = "fpcalc.exe"
	}
	return filepath.Join(dir, name), nil
}

// ResolveFpcalc finds a usable fpcalc binary: PATH → cache dir → download.
func ResolveFpcalc() (string, error) {
	if p, err := exec.LookPath("fpcalc"); err == nil {
		return p, nil
	}
	dest, err := FpcalcCachePath()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}
	return downloadFpcalc(dest)
}

func downloadFpcalc(dest string) (string, error) {
	url, isZip, err := fpcalcDownloadURL()
	if err != nil {
		return "", err
	}

	fmt.Fprintf(os.Stderr, "fpcalc not found — downloading chromaprint %s...\n", fpcalcVersion)

	resp, err := fpcalcDLClient.Get(url)
	if err != nil {
		return "", fmt.Errorf("fpcalc download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fpcalc download failed: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFpcalcArchiveSize))
	if err != nil {
		return "", fmt.Errorf("fpcalc download read failed: %w", err)
	}

	name := "fpcalc"
	if runtime.GOOS == "windows" {
		name = "fpcalc.exe"
	}

	var binary []byte
	if isZip {
		binary, err = extractFromZip(data, name)
	} else {
		binary, err = extractFromTarGz(data, name)
	}
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("cannot create cache directory: %w", err)
	}
	if err := os.WriteFile(dest, binary, 0o755); err != nil {
		return "", fmt.Errorf("cannot write fpcalc binary: %w", err)
	}

	fmt.Fprintf(os.Stderr, "fpcalc installed to %s\n", dest)
	return dest, nil
}

func extractFromTarGz(data []byte, target string) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(data)))
	if err != nil {
		return nil, fmt.Errorf("cannot open downloaded archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("cannot read archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == target {
			return io.ReadAll(io.LimitReader(tr, maxFpcalcArchiveSize))
		}
	}
	return nil, fmt.Errorf("%s not found in downloaded archive", target)
}
