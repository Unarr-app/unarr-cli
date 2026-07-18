package upgrade

import (
	"bufio"
	"bytes"
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
func downloadWithRetry(ctx context.Context, version string, onProgress func(string)) (string, string, error) {
	var lastErr error
	for attempt := 1; attempt <= maxDownloadRetries; attempt++ {
		path, base, err := download(ctx, version)
		if err == nil {
			return path, base, nil
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
				return "", "", ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return "", "", lastErr
}

// getReleaseAsset resolves a release asset by trying each host in assetBases()
// until one returns 200 — primary (GitHub) first, then the Hetzner-backed
// fallback. It returns the live response (caller closes Body) AND the base that
// served it, so the rest of the update fetches checksums.txt + checksums.txt.sig
// from that SAME mirror. The two builds (GitHub Actions and local ship) are NOT
// guaranteed byte-identical across mirrors, so a checksums.txt from one host
// must never be verified against an archive from another. The last host's
// non-200 status or transport error is reported when every host fails.
func getReleaseAsset(ctx context.Context, version, filename string) (*http.Response, string, error) {
	var lastErr error
	for _, base := range assetBases() {
		resp, err := getReleaseAssetFromBase(ctx, base, version, filename)
		if err != nil {
			lastErr = err
			continue
		}
		return resp, base, nil
	}
	return nil, "", lastErr
}

// getReleaseAssetFromBase GETs a release asset from ONE specific base, with no
// failover. Used once the archive has pinned the mirror, so checksums.txt and
// checksums.txt.sig come from the same host as the binary they describe. Returns
// the live response on 200 (caller closes Body); otherwise an error carrying the
// non-200 status or transport failure.
func getReleaseAssetFromBase(ctx context.Context, base, version, filename string) (*http.Response, error) {
	url := releaseURL(base, version, filename)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "unarr-updater")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	resp.Body.Close()
	return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
}

// download fetches the release archive to a temporary file (primary host, then
// the Hetzner-backed fallback) and returns the temp path plus the base that
// served the archive — the caller verifies checksums + signature against that
// same mirror.
func download(ctx context.Context, version string) (string, string, error) {
	resp, base, err := getReleaseAsset(ctx, version, archiveName(version))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	tmp, err := os.CreateTemp("", "unarr-download-*.tmp")
	if err != nil {
		return "", "", err
	}
	defer tmp.Close()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		os.Remove(tmp.Name())
		return "", "", fmt.Errorf("write archive: %w", err)
	}

	return tmp.Name(), base, nil
}

// verifyChecksum downloads checksums.txt and verifies the archive's SHA256.
// When a release public key is embedded at build time (releasePubKeyBase64),
// the function also verifies an ed25519 signature over checksums.txt before
// trusting any hash inside it — this turns the checksum file from a passive
// integrity check into an authenticated artifact that a maintainer or CI key
// compromise cannot trivially forge.
func verifyChecksum(ctx context.Context, version, archivePath, base string) error {
	return verifyChecksumWithOptions(ctx, version, archivePath, base, true)
}

// verifyChecksumOnly skips the ed25519 signature step. Used by Upgrader
// when --allow-unsigned is set and the release is known to predate signing
// (or when a release accidentally shipped without a .sig file).
func verifyChecksumOnly(ctx context.Context, version, archivePath, base string) error {
	return verifyChecksumWithOptions(ctx, version, archivePath, base, false)
}

// verifyChecksumWithOptions verifies archivePath against checksums.txt fetched
// from `base` — the SAME mirror that served the archive (resolved by download).
// Fetching checksums + signature from that one host guarantees they describe the
// archive we actually downloaded, never another mirror's (possibly differently
// built) artifacts.
func verifyChecksumWithOptions(ctx context.Context, version, archivePath, base string, verifySignature bool) error {
	// Download checksums.txt from the archive's mirror (no cross-host failover).
	resp, err := getReleaseAssetFromBase(ctx, base, version, "checksums.txt")
	if err != nil {
		return fmt.Errorf("fetch checksums: %w", err)
	}
	defer resp.Body.Close()

	// Read the entire checksums.txt content first so we can both parse and
	// verify the signature over the same bytes.
	checksumsContent, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read checksums: %w", err)
	}

	// Verify ed25519 signature over checksums.txt before trusting its
	// contents. Skipped silently when no key is embedded (handled by the
	// caller via SignatureVerificationConfigured) or when the caller
	// explicitly opts out via --allow-unsigned.
	if verifySignature {
		if err := verifyChecksumsSignature(ctx, version, base, checksumsContent); err != nil {
			return fmt.Errorf("verify signature: %w", err)
		}
	}

	// Parse checksums.txt — format: "<sha256>  <filename>"
	expectedName := archiveName(version)
	var expectedHash string

	scanner := bufio.NewScanner(bytes.NewReader(checksumsContent))
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

// fetchLatestVersion returns the newest published release version (no leading
// "v"). For the default GitHub origin it queries the Releases API list endpoint
// — which, unlike releases/latest, includes prereleases, so the `-beta` channel
// resolves. For a custom base (staging / tests) it falls back to the plain
// `{base}/version` text endpoint a web origin serves.
func fetchLatestVersion(ctx context.Context) (string, error) {
	if isGitHubBase() {
		v, err := fetchLatestVersionGitHub(ctx)
		if err == nil {
			return v, nil
		}
		if fallbackBaseURL == "" {
			return "", err
		}
		// GitHub unreachable (outage / account takedown) → fail over to the
		// Hetzner-backed marker the web origin serves.
		return fetchLatestVersionText(ctx, fallbackBaseURL)
	}
	// Primary overridden to a non-GitHub origin (staging/tests): use its marker.
	return fetchLatestVersionText(ctx, updateBaseURL)
}

// fetchLatestVersionGitHub reads the newest release tag from the GitHub REST
// API. The list endpoint returns releases newest-first and includes
// prereleases (releases/latest would skip every `-beta`). Unauthenticated GitHub
// API calls are rate-limited per IP (60/hr) — the caller caches the result.
func fetchLatestVersionGitHub(ctx context.Context) (string, error) {
	url := githubAPIBase + "/releases?per_page=100"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "unarr-updater")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github releases API: HTTP %d", resp.StatusCode)
	}

	var releases []struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&releases); err != nil {
		return "", fmt.Errorf("decode releases: %w", err)
	}
	tags := make([]string, 0, len(releases))
	for _, r := range releases {
		tags = append(tags, r.TagName)
	}
	// The releases endpoint is not semver-ordered (nor reliably created-at
	// ordered), so the newest version must be chosen client-side.
	best := pickLatestVersion(tags)
	if best == "" {
		return "", fmt.Errorf("no releases found at %s", url)
	}
	return best, nil
}

// pickLatestVersion returns the highest version (no leading "v") among the tag
// names, comparing major.minor.patch and ignoring any prerelease suffix.
func pickLatestVersion(tags []string) string {
	best := ""
	for _, t := range tags {
		v := strings.TrimPrefix(strings.TrimSpace(t), "v")
		if v == "" {
			continue
		}
		if best == "" || versionLess(best, v) {
			best = v
		}
	}
	return best
}

// versionLess reports whether a < b by major.minor.patch. Prerelease suffixes
// are ignored (parseInt-style: each segment is read up to its first non-digit),
// matching the agent-version comparison on the web.
func versionLess(a, b string) bool {
	a0, a1, a2 := splitVersion(a)
	b0, b1, b2 := splitVersion(b)
	if a0 != b0 {
		return a0 < b0
	}
	if a1 != b1 {
		return a1 < b1
	}
	return a2 < b2
}

func splitVersion(v string) (int, int, int) {
	parts := strings.SplitN(v, ".", 3)
	seg := func(i int) int {
		if i >= len(parts) {
			return 0
		}
		n := 0
		for _, c := range parts[i] {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	return seg(0), seg(1), seg(2)
}

// fetchLatestVersionText reads a plain "vX.Y.Z" string from `{base}/version` —
// the endpoint a staging / mirror / Hetzner-backed web origin serves.
func fetchLatestVersionText(ctx context.Context, base string) (string, error) {
	url := base + "/version"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "unarr-updater")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch latest version: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("version endpoint: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", fmt.Errorf("read version: %w", err)
	}

	version := strings.TrimPrefix(strings.TrimSpace(string(body)), "v")
	if version == "" {
		return "", fmt.Errorf("empty version from %s", url)
	}

	return version, nil
}
