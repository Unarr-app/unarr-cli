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
// To bump: pick a newer tag, copy each asset's SHA-256 from the release body
// (https://github.com/cloudflare/cloudflared/releases/tag/<version>) into the
// map below, and update this constant. EVERY asset named by assetFor must have
// an entry, or the download refuses to run for that platform.
const pinnedCloudflaredVersion = "2026.5.2"

// pinnedCloudflaredSHA256 maps each downloadable asset to its SHA-256 for
// pinnedCloudflaredVersion (from the release body — Cloudflare publishes the
// hashes inline there, not as a separate file or signature).
//
// WINDOWS IS HERE, macOS IS NOT, and the difference is the shape of the asset,
// not a judgement about the platform. Cloudflare ships Windows as a bare .exe,
// which this can verify and drop into place exactly like a Linux binary. The
// darwin assets are .tgz archives, so supporting them means unpacking an archive
// downloaded from the internet before anything has been verified — a different
// and larger piece of work than a hash check, and `brew install cloudflared` is
// the path a Mac user already has.
//
// windows/arm64 and windows/386 are absent on purpose: upstream publishes no
// arm64 asset for this release, and this project does not build a 386 target
// (.goreleaser.yml: amd64, arm64). Both fall through to the manual-install
// message rather than being given a hash nobody has verified.
var pinnedCloudflaredSHA256 = map[string]string{
	"cloudflared-linux-amd64":       "5286698547f03df745adb2355f04c12dde52ef425491e81f433642d695521886",
	"cloudflared-linux-arm64":       "5a4e8ce2701105271412059f44b6a0bf1ae4542b4d98ff3180c0c019443a5815",
	"cloudflared-linux-armhf":       "190152c373f608080eb6aa9e2aad395f88398dfb9efd0f9b064e2652cffcefdd",
	"cloudflared-linux-386":         "ad82d1dbed8bbb9d702807cbd97df932cc774d29e9da5c109b7a3c7f7aee2065",
	"cloudflared-windows-amd64.exe": "20b9638f685333d623798e733effbad2487093f15ba592f6c7752360ff3b7ab7",
}

// ErrNoAutoDownload marks the one funnel failure that RETRYING CANNOT FIX:
// there is no cloudflared on this machine and this platform has no auto-download
// path, so the only thing that changes the outcome is a human installing the
// binary. It is a sentinel (errors.Is) rather than a string match so the
// supervisor can stop instead of logging the same line every five minutes
// forever — which is what it did, 288 times a day, on every Windows box with the
// funnel enabled, pushing real evidence out of the crash-report log tail.
var ErrNoAutoDownload = errors.New("funnel: cloudflared is not installed and cannot be downloaded automatically on this platform")

// ResolveBinary returns the path to a usable cloudflared executable, downloading
// one into the unarr data dir if neither $PATH nor the cached location has it.
// This makes the funnel feature usable on headless installs (NAS / Docker)
// where the user can't easily install cloudflared via the OS package manager.
//
// Resolution order:
//
//  1. cloudflared on $PATH (operator already installed it)
//  2. <data-dir>/bin/cloudflared (we cached it on a previous run)
//  3. download from GitHub releases, where an asset exists for this platform
//     (linux, and windows/amd64). Everything else returns ErrNoAutoDownload
//     naming the OS package-manager command instead.
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

// installHint is the per-OS one-liner that actually gets a user unstuck. The
// old message said only "install cloudflared manually", which on Windows is a
// dead end unless you already know the package name.
//
// ASCII ONLY. This text ends up in unarr.log, in a crash report, and on a
// Windows console — three decoders, and the ones that are not UTF-8 turn a
// single em dash into mojibake. The field reports show exactly that ("windows
// ?" install cloudflared"), which is how a legible instruction became noise.
func installHint() string {
	switch runtime.GOOS {
	case "windows":
		return "winget install --id Cloudflare.cloudflared"
	case "darwin":
		return "brew install cloudflared"
	default:
		return "see https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/"
	}
}

// assetFor names the release asset for a platform, or reports that there is
// none to download. Keep it in step with pinnedCloudflaredSHA256: an asset named
// here without a hash there is refused at the next step rather than fetched
// unverified, which is the safe direction but still a bug.
//
// Only asset shapes that are a BARE EXECUTABLE appear. Cloudflare also ships
// .tgz (darwin), .msi and .deb/.rpm, and every one of those would mean running
// an unpacker or an installer over bytes fetched from the internet. The hash is
// checked before the file is promoted or executed, and that guarantee is only
// worth something while "promote" means "rename a verified file into place".
func assetFor(goos, goarch string) (string, bool) {
	switch goos {
	case "linux":
		switch goarch {
		case "amd64":
			return "cloudflared-linux-amd64", true
		case "arm64":
			return "cloudflared-linux-arm64", true
		case "arm":
			return "cloudflared-linux-armhf", true
		case "386":
			return "cloudflared-linux-386", true
		}
	case "windows":
		// amd64 only: upstream publishes no windows/arm64 asset for the pinned
		// release, and this project ships no 386 build to need one.
		if goarch == "amd64" {
			return "cloudflared-windows-amd64.exe", true
		}
	}
	return "", false
}

func cachedBinaryPath() string {
	name := "cloudflared"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(config.DataDir(), "bin", name)
}

// downloadCloudflared fetches the PINNED cloudflared release asset matching the
// current GOOS/GOARCH into `dest`. Platforms with no bare-executable asset (see
// assetFor) get ErrNoAutoDownload pointing at their package manager.
//
// Integrity: the fetch is HTTPS (bounded by Let's Encrypt + GitHub's cert
// chain) AND the downloaded bytes are verified against a baked-in SHA-256 for
// the pinned version (pinnedCloudflaredSHA256). A mismatch — corruption, MITM
// past TLS, a swapped asset — is rejected before the binary is promoted or run.
// Because the version is pinned (not `latest`), a future malicious/breaking
// upstream release is never pulled silently. The cheap magic-byte/size check
// still runs first to reject a 404 HTML page before hashing 50 MB. For stricter
// control, install cloudflared via your distro package manager — the PATH copy
// always takes precedence.
func downloadCloudflared(dest string) (string, error) {
	asset, ok := assetFor(runtime.GOOS, runtime.GOARCH)
	if !ok {
		// Wrapped in ErrNoAutoDownload so the supervisor can tell "a human has to
		// act" from "the CF edge is having a bad minute" and stop retrying. Also
		// ASCII-only: this string is logged, and a UTF-8 em dash in the log is
		// what a Windows console (CP850) and a CP1252 reader each mangle
		// differently — see the note on installHint.
		return "", fmt.Errorf("%w for %s/%s: install it manually (%s) or drop a binary at %s",
			ErrNoAutoDownload, runtime.GOOS, runtime.GOARCH, installHint(), dest)
	}

	expectedSHA, ok := pinnedCloudflaredSHA256[asset]
	if !ok {
		return "", fmt.Errorf("funnel: no pinned SHA-256 for asset %q (bug: keep pinnedCloudflaredSHA256 in sync with assetFor)", asset)
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

	// Cheap reject first: must look like a native executable (rejects 404 HTML
	// pages or wrong-arch payloads) and be at least 1 MB, so we don't hash 50 MB
	// of an obviously-wrong file.
	if err := verifyExecutable(tmp); err != nil {
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

// verifyExecutable returns nil when the file at `path` looks like a native
// executable for this platform and is at least 1 MB. A low-cost guard against
// an HTML error page or a wrong-arch payload, run BEFORE hashing 50 MB.
//
// It is a sanity check, not the security boundary — verifySHA256 is that. Which
// is why "unknown platform" is not an error here: a magic-byte table that has
// not been taught about a new GOOS must not block a download whose bytes are
// about to be proven correct anyway.
func verifyExecutable(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Size() < 1024*1024 {
		return errors.New("file is suspiciously small (<1 MB)")
	}
	magic, ok := executableMagic(runtime.GOOS)
	if !ok {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, len(magic))
	if _, err := io.ReadFull(f, head); err != nil {
		return fmt.Errorf("read magic bytes: %w", err)
	}
	if !bytes.Equal(head, magic) {
		return fmt.Errorf("not a native %s executable", runtime.GOOS)
	}
	return nil
}

// executableMagic is the leading byte signature of a native executable: ELF on
// linux, the DOS "MZ" stub every PE still starts with on Windows.
func executableMagic(goos string) ([]byte, bool) {
	switch goos {
	case "linux":
		return []byte{0x7f, 'E', 'L', 'F'}, true
	case "windows":
		return []byte{'M', 'Z'}, true
	}
	return nil, false
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
