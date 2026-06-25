package funnel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// pinnedCloudflaredVersion is the exact cloudflared release the auto-downloader
// fetches. We deliberately do NOT track `latest`: pinning a version we vetted +
// verifying its SHA-256 is what bounds the supply-chain risk (a future malicious
// or breaking upstream release can't be pulled silently). Operator-installed
// cloudflared on $PATH always wins, so this only affects the headless
// auto-download fallback.
//
// To bump: pick a newer tag, copy its per-asset SHA-256 from the release body
// (https://github.com/cloudflare/cloudflared/releases/tag/<version>) into the
// map below, and update this constant. All four arch entries MUST be present.
const pinnedCloudflaredVersion = "2026.5.2"

// pinnedCloudflaredSHA256 maps each linux asset to its SHA-256 for
// pinnedCloudflaredVersion (from the release body — Cloudflare publishes the
// hashes inline there, not as a separate file or signature).
var pinnedCloudflaredSHA256 = map[string]string{
	"cloudflared-linux-amd64": "5286698547f03df745adb2355f04c12dde52ef425491e81f433642d695521886",
	"cloudflared-linux-arm64": "5a4e8ce2701105271412059f44b6a0bf1ae4542b4d98ff3180c0c019443a5815",
	"cloudflared-linux-armhf": "190152c373f608080eb6aa9e2aad395f88398dfb9efd0f9b064e2652cffcefdd",
	"cloudflared-linux-386":   "ad82d1dbed8bbb9d702807cbd97df932cc774d29e9da5c109b7a3c7f7aee2065",
}

// ResolveBinary returns the path to a usable cloudflared executable, downloading
// one into the unarr data dir if neither $PATH nor the cached location has it.
// This makes the funnel feature usable on headless installs (NAS / Docker)
// where the user can't easily install cloudflared via the OS package manager.
//
// Resolution order:
//
//  1. cloudflared on $PATH (operator already installed it)
//  2. <data-dir>/bin/cloudflared (we cached it on a previous run)
//  3. download from GitHub releases (Linux-only fallback; macOS / Windows
//     return a clear error pointing at brew / winget)
func ResolveBinary() (string, error) {
	if p, err := exec.LookPath("cloudflared"); err == nil {
		return p, nil
	}
	cached := cachedBinaryPath()
	if _, err := os.Stat(cached); err == nil {
		return cached, nil
	}
	return downloadCloudflared(cached)
}

func cachedBinaryPath() string {
	name := "cloudflared"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(config.DataDir(), "bin", name)
}

// downloadCloudflared fetches the PINNED cloudflared release asset matching the
// current GOOS/GOARCH into `dest`. Linux only — macOS/Windows return a pointer
// at the OS package manager.
//
// Integrity: the fetch is HTTPS (bounded by Let's Encrypt + GitHub's cert
// chain) AND the downloaded bytes are verified against a baked-in SHA-256 for
// the pinned version (pinnedCloudflaredSHA256). A mismatch — corruption, MITM
// past TLS, a swapped asset — is rejected before the binary is promoted or run.
// Because the version is pinned (not `latest`), a future malicious/breaking
// upstream release is never pulled silently. The cheap ELF/size check still
// runs first to reject a 404 HTML page before hashing 50 MB. For stricter
// control, install cloudflared via your distro package manager — the PATH copy
// always takes precedence.
func downloadCloudflared(dest string) (string, error) {
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("funnel: auto-download not supported on %s — install cloudflared manually or drop a binary at %s", runtime.GOOS, dest)
	}

	var asset string
	switch runtime.GOARCH {
	case "amd64":
		asset = "cloudflared-linux-amd64"
	case "arm64":
		asset = "cloudflared-linux-arm64"
	case "arm":
		asset = "cloudflared-linux-armhf"
	case "386":
		asset = "cloudflared-linux-386"
	default:
		return "", fmt.Errorf("funnel: unsupported linux arch %q — install cloudflared manually", runtime.GOARCH)
	}

	expectedSHA, ok := pinnedCloudflaredSHA256[asset]
	if !ok {
		return "", fmt.Errorf("funnel: no pinned SHA-256 for asset %q (bug: keep pinnedCloudflaredSHA256 in sync with the arch switch)", asset)
	}

	url := "https://github.com/cloudflare/cloudflared/releases/download/" + pinnedCloudflaredVersion + "/" + asset
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("funnel: create bin dir: %w", err)
	}

	// O_EXCL so concurrent unarr-dev / prod daemons don't clobber each
	// other's partial download. The loser gets EEXIST → falls back to
	// polling for the winner to finish.
	tmp := dest + ".partial"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// Another process is downloading. Wait briefly for them to finish.
			for range 60 {
				time.Sleep(time.Second)
				if _, statErr := os.Stat(dest); statErr == nil {
					return dest, nil
				}
			}
			return "", fmt.Errorf("funnel: another download in progress at %s (timed out)", tmp)
		}
		return "", fmt.Errorf("funnel: open dest: %w", err)
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("funnel: download cloudflared: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = out.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("funnel: download cloudflared: HTTP %d from %s", resp.StatusCode, url)
	}

	if _, err := io.Copy(out, resp.Body); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("funnel: write dest: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("funnel: close dest: %w", err)
	}

	// Cheap reject first: must be a Linux ELF executable (rejects 404 HTML pages
	// or wrong-arch payloads) and at least 1 MB, so we don't hash 50 MB of an
	// obviously-wrong file.
	if err := verifyLinuxElf(tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("funnel: downloaded file failed sanity check: %w", err)
	}

	// Authoritative integrity gate: the bytes must match the SHA-256 we baked in
	// for the pinned version. Rejects corruption, a MITM past TLS, or a swapped
	// asset before the binary is ever promoted or executed.
	if err := verifySHA256(tmp, expectedSHA); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("funnel: cloudflared %s integrity check failed: %w", pinnedCloudflaredVersion, err)
	}

	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("funnel: rename dest: %w", err)
	}
	return dest, nil
}

// verifyLinuxElf returns nil when the file at `path` starts with the ELF
// magic bytes and is at least 1 MB. Used as a low-cost guard against
// downloading an HTML error page or a wrong-arch payload.
func verifyLinuxElf(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Size() < 1024*1024 {
		return errors.New("file is suspiciously small (<1 MB)")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		return fmt.Errorf("read magic bytes: %w", err)
	}
	if !bytes.Equal(head, []byte{0x7f, 'E', 'L', 'F'}) {
		return errors.New("not an ELF binary")
	}
	return nil
}

// verifySHA256 returns nil when the file at `path` hashes to expectedHex
// (case-insensitive), else an error reporting both digests.
func verifySHA256(path, expectedHex string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hashing: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, expectedHex) {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, expectedHex)
	}
	return nil
}
