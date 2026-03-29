package mediainfo

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

var (
	ffprobeAPIClient = &http.Client{Timeout: 30 * time.Second}
	ffprobeDLClient  = &http.Client{Timeout: 10 * time.Minute}
)

const maxFFprobeZipSize = 100 * 1024 * 1024 // 100MB

const ffbinariesAPI = "https://ffbinaries.com/api/v1/version/latest"

type ffbinariesResponse struct {
	Version string                       `json:"version"`
	Bin     map[string]map[string]string `json:"bin"`
}

// ffprobePlatformKey maps GOOS/GOARCH to ffbinaries platform keys.
func ffprobePlatformKey() (string, error) {
	switch runtime.GOOS {
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			return "linux-64", nil
		case "arm64":
			return "linux-arm64", nil
		}
	case "darwin":
		return "osx-64", nil
	case "windows":
		if runtime.GOARCH == "amd64" {
			return "windows-64", nil
		}
	}
	return "", fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
}

// FFprobeCacheDir returns the directory where the downloaded ffprobe binary is stored.
func FFprobeCacheDir() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "unarr", "bin"), nil
}

// FFprobeCachePath returns the full path to the cached ffprobe binary.
func FFprobeCachePath() (string, error) {
	dir, err := FFprobeCacheDir()
	if err != nil {
		return "", err
	}
	name := "ffprobe"
	if runtime.GOOS == "windows" {
		name = "ffprobe.exe"
	}
	return filepath.Join(dir, name), nil
}

// DownloadFFprobe downloads a static ffprobe binary for the current platform
// and caches it locally. Returns the path to the binary.
func DownloadFFprobe() (string, error) {
	dest, err := FFprobeCachePath()
	if err != nil {
		return "", fmt.Errorf("cannot determine cache path: %w", err)
	}

	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	platform, err := ffprobePlatformKey()
	if err != nil {
		return "", err
	}

	url, err := resolveFFprobeURL(platform)
	if err != nil {
		return "", err
	}

	fmt.Fprintf(os.Stderr, "ffprobe not found — downloading for %s...\n", platform)

	resp, err := ffprobeDLClient.Get(url)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	zipData, err := io.ReadAll(io.LimitReader(resp.Body, maxFFprobeZipSize))
	if err != nil {
		return "", fmt.Errorf("download read failed: %w", err)
	}

	name := "ffprobe"
	if runtime.GOOS == "windows" {
		name = "ffprobe.exe"
	}

	binary, err := extractFromZip(zipData, name)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("cannot create cache directory: %w", err)
	}

	if err := os.WriteFile(dest, binary, 0o755); err != nil {
		return "", fmt.Errorf("cannot write ffprobe binary: %w", err)
	}

	fmt.Fprintf(os.Stderr, "ffprobe installed to %s\n", dest)
	return dest, nil
}

func resolveFFprobeURL(platform string) (string, error) {
	resp, err := ffprobeAPIClient.Get(ffbinariesAPI)
	if err != nil {
		return "", fmt.Errorf("cannot reach ffbinaries.com: %w", err)
	}
	defer resp.Body.Close()

	var data ffbinariesResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", fmt.Errorf("cannot parse ffbinaries response: %w", err)
	}

	bins, ok := data.Bin[platform]
	if !ok {
		return "", fmt.Errorf("no ffprobe binary available for platform %q", platform)
	}

	url, ok := bins["ffprobe"]
	if !ok {
		return "", fmt.Errorf("no ffprobe download URL for platform %q", platform)
	}

	return url, nil
}

func extractFromZip(data []byte, target string) ([]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("cannot open downloaded archive: %w", err)
	}

	for _, f := range r.File {
		if filepath.Base(f.Name) == target {
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("cannot extract %s from archive: %w", target, err)
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}

	return nil, fmt.Errorf("%s not found in downloaded archive", target)
}
