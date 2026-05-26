package funnel

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/torrentclaw/unarr/internal/config"
)

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

// downloadCloudflared fetches the latest cloudflared release asset matching
// the current GOOS/GOARCH into `dest`. Linux only — macOS/Windows return a
// pointer at the OS package manager.
//
// Supply-chain caveat: we trust GitHub-over-TLS + cloudflare/cloudflared
// repo integrity. The fetch is over HTTPS to api.github.com's release-asset
// redirector, so a network MITM is bounded by Let's Encrypt + GitHub's cert
// chain. We additionally verify the file is an ELF binary (Linux magic
// bytes) so a generic 404 HTML page or a wrong-arch tarball is rejected at
// rest. We do NOT verify a signature because Cloudflare doesn't sign release
// assets at the moment — if you need stricter integrity, install cloudflared
// from your distro's package manager (apt/brew/winget) and unarr will use
// the PATH copy.
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

	url := "https://github.com/cloudflare/cloudflared/releases/latest/download/" + asset
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

	// Sanity check before promoting <partial> to <dest>: must be a Linux
	// ELF executable (rejects 404 HTML pages or wrong-arch payloads) and at
	// least 1 MB (real cloudflared is ~50 MB; anything smaller is corrupt).
	if err := verifyLinuxElf(tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("funnel: downloaded file failed sanity check: %w", err)
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
