package mediainfo

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

const maxFFmpegZipSize = 200 * 1024 * 1024 // 200MB — ffmpeg static is ~70-100MB compressed

// FFmpegCachePath returns the full path to the cached ffmpeg binary
// (sibling of the cached ffprobe binary).
func FFmpegCachePath() (string, error) {
	dir, err := FFprobeCacheDir()
	if err != nil {
		return "", err
	}
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.exe"
	}
	return filepath.Join(dir, name), nil
}

// DownloadFFmpeg downloads a static ffmpeg binary for the current platform
// and caches it locally. Returns the path to the binary. Reuses
// resolveFFprobeURL's ffbinaries.com discovery endpoint — that index ships
// both ffprobe and ffmpeg per platform.
func DownloadFFmpeg() (string, error) {
	dest, err := FFmpegCachePath()
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

	url, err := resolveFFmpegURL(platform)
	if err != nil {
		return "", err
	}

	fmt.Fprintf(os.Stderr, "ffmpeg not found — downloading for %s (~70MB)...\n", platform)

	resp, err := ffprobeDLClient.Get(url)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	zipData, err := io.ReadAll(io.LimitReader(resp.Body, maxFFmpegZipSize))
	if err != nil {
		return "", fmt.Errorf("download read failed: %w", err)
	}

	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.exe"
	}

	binary, err := extractFromZip(zipData, name)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("cannot create cache directory: %w", err)
	}

	if err := os.WriteFile(dest, binary, 0o755); err != nil {
		return "", fmt.Errorf("cannot write ffmpeg binary: %w", err)
	}

	fmt.Fprintf(os.Stderr, "ffmpeg installed to %s\n", dest)
	return dest, nil
}

// resolveFFmpegURL fetches the ffbinaries index and returns the ffmpeg
// download URL for the requested platform key (e.g. "linux-64").
func resolveFFmpegURL(platform string) (string, error) {
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
		return "", fmt.Errorf("no ffmpeg binary available for platform %q", platform)
	}

	url, ok := bins["ffmpeg"]
	if !ok {
		return "", fmt.Errorf("no ffmpeg download URL for platform %q", platform)
	}

	return url, nil
}
