package upgrade

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 120 * time.Second}

const (
	maxDownloadRetries = 3
	retryBaseDelay     = 5 * time.Second
)

// retryDelays returns the wait duration before the nth retry (1-based).
// Delays: 5s, 15s — increasing gap to avoid hammering on transient failures.
func retryDelay(attempt int) time.Duration {
	return retryBaseDelay * time.Duration(attempt*attempt)
}

// downloadWithRetry fetches the release archive, retrying on transient errors.
// onProgress is called with user-facing messages (may be nil).
func downloadWithRetry(ctx context.Context, version string, onProgress func(string)) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= maxDownloadRetries; attempt++ {
		path, err := download(ctx, version)
		if err == nil {
			return path, nil
		}
		lastErr = err
		if attempt < maxDownloadRetries {
			delay := retryDelay(attempt)
			if onProgress != nil {
				onProgress(fmt.Sprintf("Download failed (%v)", err))
				onProgress(fmt.Sprintf("Retrying in %s... (attempt %d/%d)", delay, attempt+1, maxDownloadRetries))
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return "", lastErr
}

// download fetches the release archive to a temporary file.
func download(ctx context.Context, version string) (string, error) {
	url := releaseURL(version, archiveName(version))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "unarr-updater")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "unarr-download-*.tmp")
	if err != nil {
		return "", err
	}
	defer tmp.Close()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("write archive: %w", err)
	}

	return tmp.Name(), nil
}

// verifyChecksum downloads checksums.txt and verifies the archive's SHA256.
func verifyChecksum(ctx context.Context, version, archivePath string) error {
	// Download checksums.txt
	url := releaseURL(version, "checksums.txt")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "unarr-updater")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch checksums: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch checksums: HTTP %d", resp.StatusCode)
	}

	// Parse checksums.txt — format: "<sha256>  <filename>"
	expectedName := archiveName(version)
	var expectedHash string

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[1] == expectedName {
			expectedHash = parts[0]
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read checksums: %w", err)
	}

	if expectedHash == "" {
		return fmt.Errorf("no checksum found for %s in checksums.txt", expectedName)
	}

	// Compute SHA256 of the downloaded archive
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash archive: %w", err)
	}

	actualHash := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(actualHash, expectedHash) {
		return fmt.Errorf("SHA256 mismatch: expected %s, got %s", expectedHash, actualHash)
	}

	return nil
}

// fetchLatestVersion queries GitHub API for the latest release tag.
func fetchLatestVersion(ctx context.Context) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "unarr-updater")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API: HTTP %d", resp.StatusCode)
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if release.TagName == "" {
		return "", fmt.Errorf("empty tag_name in release")
	}

	return strings.TrimPrefix(release.TagName, "v"), nil
}
