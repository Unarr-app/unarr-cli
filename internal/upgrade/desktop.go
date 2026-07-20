package upgrade

// Desktop companion (unarr-desktop) updates.
//
// The desktop binaries are RAW per-OS executables attached to the same GitHub
// Release by .github/workflows/desktop.yml — they are NOT covered by
// goreleaser's checksums.txt (which only lists the CLI archives), so they
// carry their own signed manifest, produced by desktop.yml's sign job:
//
//	checksums-desktop.txt      "<sha256>  unarr-desktop_<ver>_<os>_<arch>[.exe]" per line
//	checksums-desktop.txt.sig  ed25519 signature by the SAME release key
//
// Verification trusts the same compiled-in public key as the CLI updater
// (releasePubKeyBase64). There is deliberately NO allow-unsigned escape hatch
// here: unlike the CLI (which had a pre-signing release history to allow
// downgrading into), desktop self-update starts signed — a release without a
// valid signed manifest simply cannot be installed by this code path.
//
// Consumers:
//   - `unarr update` refreshes the unarr-desktop binary installed NEXT TO the
//     CLI (UpdateDesktopSibling) after updating itself.
//   - `unarr-desktop --update` self-updates player-only installs that have no
//     CLI at all (UpdateDesktopBinary against its own executable).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const desktopBinaryName = "unarr-desktop"

// desktopChecksums is the signed manifest covering the desktop assets (its
// signature asset is derived as "<manifest>.sig", see sigAssetName).
const desktopChecksums = "checksums-desktop.txt"

// desktopVersionMarker is the CROSS-REPO contract for "which unarr-desktop is
// installed here": a plain-text file with the installed version, living next
// to the desktop binary. Written by this updater after every successful swap
// AND by the web's installers (install.sh / install.ps1 render) — the name
// must stay EXACTLY in sync with the web repo. It exists because the binary
// itself can never be probed safely: pre-1.6 desktop builds treat ANY argv as
// tray mode, so executing an unknown old binary with --version could pop a
// tray (or hang a headless session). A file we can read beats a binary we
// must run.
const desktopVersionMarker = "unarr-desktop.version"

func desktopVersionMarkerPath(dir string) string {
	return filepath.Join(dir, desktopVersionMarker)
}

// writeDesktopVersionMarker records the installed desktop version next to the
// binary. Plain text, no trailing newline (readers tolerate trailing
// whitespace, so a hand-edited or installer-written file with one still
// parses).
func writeDesktopVersionMarker(dir, version string) error {
	return os.WriteFile(desktopVersionMarkerPath(dir), []byte(strings.TrimPrefix(version, "v")), 0o644)
}

// InstalledDesktopVersion reads the version marker next to the desktop binary
// in dir, best-effort: (version, true) when present and sane, ("", false)
// when absent or corrupt. Corrupt/absent means "unknown", NEVER a guess — the
// caller then proceeds with a full (idempotent) update, which is the safe
// direction: worst case we re-download a binary we already had, we never skip
// an update we needed nor execute an old binary to find out.
func InstalledDesktopVersion(dir string) (string, bool) {
	data, err := os.ReadFile(desktopVersionMarkerPath(dir))
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(string(data))
	// A version is one short token: embedded whitespace/newlines or an absurd
	// length mean the file is not (only) a version marker — treat as corrupt.
	if v == "" || len(v) > 64 || strings.ContainsAny(v, " \t\r\n") {
		return "", false
	}
	return strings.TrimPrefix(v, "v"), true
}

// ErrNoDesktopAssets marks a release that ships no (signed) desktop assets —
// either the binary asset or the checksums-desktop manifest is absent. Old
// releases predate desktop signing, and a release published moments ago may
// not have its desktop.yml assets attached yet (that workflow runs AFTER the
// release is published). `unarr update` warns and continues on this error;
// `unarr-desktop --update` surfaces it to the user.
var ErrNoDesktopAssets = errors.New("no signed desktop assets for this release")

// IsNewer reports whether candidate is strictly newer than current, comparing
// major.minor.patch and ignoring prerelease suffixes — the same semantics as
// the web's agent-version gate, so every surface agrees on "outdated".
func IsNewer(current, candidate string) bool {
	return versionLess(strings.TrimPrefix(current, "v"), strings.TrimPrefix(candidate, "v"))
}

// desktopAssetName returns the release asset filename desktop.yml publishes
// for one OS/arch. MUST stay in sync with the workflow's $ASSET naming — the
// manifest entries and the download URL are both derived from it.
func desktopAssetName(version, goos, goarch string) string {
	name := fmt.Sprintf("%s_%s_%s_%s", desktopBinaryName, version, goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// desktopBinaryFilename is the on-disk name of the installed desktop binary.
func desktopBinaryFilename(goos string) string {
	if goos == "windows" {
		return desktopBinaryName + ".exe"
	}
	return desktopBinaryName
}

// FindDesktopSibling returns the path of an unarr-desktop binary living in
// dir, if present. `unarr update` uses it to refresh the tray companion the
// installers drop NEXT TO the CLI — deliberately same-dir only, never a PATH
// search: a PATH hit could belong to a different install (packaged, another
// user's prefix) that this update has no business overwriting.
func FindDesktopSibling(dir string) (string, bool) {
	p := filepath.Join(dir, desktopBinaryFilename(runtime.GOOS))
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
		return p, true
	}
	return "", false
}

// desktopSmokeTest verifies a downloaded desktop binary actually runs before
// it replaces the installed one: `unarr-desktop --version` is supported by
// every release that ships this update path (the flag and the updater landed
// together), and unlike the bare invocation it can never start a tray. A
// package var so the full update flow is testable with fixture payloads that
// aren't real executables.
var desktopSmokeTest = func(binPath, expectedVersion string) error {
	ctx, cancel := context.WithTimeout(context.Background(), smokeTestTO)
	defer cancel()
	out, err := exec.CommandContext(ctx, binPath, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to run: %w (output: %s)", err, string(out))
	}
	if !strings.Contains(string(out), expectedVersion) {
		return fmt.Errorf("version mismatch: expected %q in output %q", expectedVersion, string(out))
	}
	return nil
}

// downloadDesktopAsset fetches the raw desktop binary to a temp file and
// returns its path plus the base (mirror) that served it, so the signed
// manifest is fetched from that SAME host — mirrors are not guaranteed
// byte-identical, exactly like the CLI archive flow. The temp file keeps the
// platform's executable suffix so the smoke test can exec it on Windows.
func downloadDesktopAsset(ctx context.Context, version, assetName string) (string, string, error) {
	resp, base, err := getReleaseAsset(ctx, version, assetName)
	if err != nil {
		if errors.Is(err, errAssetNotFound) {
			return "", "", fmt.Errorf("%w (%s not published)", ErrNoDesktopAssets, assetName)
		}
		return "", "", err
	}
	defer resp.Body.Close()

	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	tmp, err := os.CreateTemp("", "unarr-desktop-update-*"+suffix)
	if err != nil {
		return "", "", err
	}
	defer tmp.Close()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		os.Remove(tmp.Name())
		return "", "", fmt.Errorf("write asset: %w", err)
	}
	return tmp.Name(), base, nil
}

// verifyDesktopAsset checks the downloaded desktop binary against the signed
// checksums-desktop manifest from the SAME mirror the asset came from.
// Signature verification is MANDATORY: a missing manifest means the release
// ships no signed desktop assets (ErrNoDesktopAssets), and a manifest without
// a valid signature is an explicit hard failure — never applied.
func verifyDesktopAsset(ctx context.Context, version, assetPath, base, assetName string) error {
	err := verifyAssetAgainstChecksums(ctx, checksumVerifyReq{
		version:         version,
		assetPath:       assetPath,
		base:            base,
		manifest:        desktopChecksums,
		expectedName:    assetName,
		verifySignature: true,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errAssetNotFound):
		// The manifest itself 404'd — release predates desktop signing (or
		// the sign job hasn't finished attaching it yet).
		return fmt.Errorf("%w (missing %s)", ErrNoDesktopAssets, desktopChecksums)
	case errors.Is(err, ErrMissingSignature):
		return fmt.Errorf("desktop checksums manifest is unsigned (%s missing) — refusing to install", sigAssetName(desktopChecksums))
	default:
		return err
	}
}

// UpdateDesktopBinary downloads, verifies, smoke-tests, and swaps the desktop
// binary at targetPath to `version`. Returns applied=true when a new binary
// was installed, applied=false when the version marker showed the install is
// already at `version` (nothing downloaded, nothing touched).
//
// The skip reads the unarr-desktop.version marker next to targetPath — never
// the binary itself (executing an unknown old desktop binary is unsafe:
// pre-1.6 builds treat any argv as tray mode). Absent/corrupt marker →
// proceed with the full update, which is idempotent and therefore always
// safe; the marker only ever saves work, it never gates correctness.
//
// The swap is rename-aside + write — the same pattern the CLI updater uses
// (its ".backup"): on unix a running tray keeps executing its old inode after
// the rename; on Windows a running exe can be RENAMED (only deleting or
// overwriting it is blocked), so parking it at ".old" frees the path for the
// new file even while the tray is up. The parked copy doubles as the rollback
// source and is left on disk as the backup.
//
// SECURITY: no binary is ever applied without the full chain — ed25519
// signature over checksums-desktop.txt with the compiled-in release key, then
// SHA256 of the downloaded asset against that manifest. Missing manifest,
// missing/invalid signature, or hash mismatch each abort with an explicit
// error; there is no unsigned fallback for desktop assets.
func UpdateDesktopBinary(ctx context.Context, targetPath, version string, logf func(string)) (bool, error) {
	version = strings.TrimPrefix(version, "v")
	say := func(msg string) {
		if logf != nil {
			logf(msg)
		}
	}
	targetDir := filepath.Dir(targetPath)
	if installed, ok := InstalledDesktopVersion(targetDir); ok && installed == version {
		say(fmt.Sprintf("unarr-desktop v%s already current — skipping download", version))
		return false, nil
	}
	assetName := desktopAssetName(version, runtime.GOOS, runtime.GOARCH)

	say(fmt.Sprintf("Downloading %s...", assetName))
	tmpPath, base, err := downloadDesktopAsset(ctx, version, assetName)
	if err != nil {
		return false, err
	}
	defer os.Remove(tmpPath)

	if SignatureVerificationConfigured() {
		say("Verifying desktop checksums + ed25519 signature...")
	} else {
		say("Verifying desktop checksums (signature verification not configured for this build)...")
	}
	if err := verifyDesktopAsset(ctx, version, tmpPath, base, assetName); err != nil {
		return false, err
	}

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return false, fmt.Errorf("chmod: %w", err)
	}
	say("Verifying new binary...")
	if err := desktopSmokeTest(tmpPath, version); err != nil {
		return false, fmt.Errorf("smoke test: %w", err)
	}

	backupPath, err := parkOldBinary(targetPath)
	if err != nil {
		return false, fmt.Errorf("backup current binary: %w", err)
	}
	if err := installBinary(tmpPath, targetPath); err != nil {
		if rbErr := os.Rename(backupPath, targetPath); rbErr != nil {
			return false, fmt.Errorf("install failed (%v) AND rollback failed (%v) — manual recovery needed at %s", err, rbErr, backupPath)
		}
		return false, fmt.Errorf("install (rolled back): %w", err)
	}
	// Record what we just installed so the next update to the same version is
	// a no-op. Best-effort AFTER the swap succeeded: a marker write failure
	// must not fail an update that is already on disk — worst case the next
	// run re-downloads.
	if err := writeDesktopVersionMarker(targetDir, version); err != nil {
		say(fmt.Sprintf("Warning: could not write %s (next update will re-download): %v", desktopVersionMarker, err))
	}
	say(fmt.Sprintf("Installed %s v%s (previous binary kept at %s)", filepath.Base(targetPath), version, backupPath))
	return true, nil
}

// parkOldBinary moves the current binary aside to <path>.old, clearing any
// stale .old from a previous update first. If that spot is unavailable (a
// Windows process from an even older update may still hold the .old file
// open, blocking both the delete and a replace-rename), fall back to a unique
// timestamped name rather than failing the whole update.
func parkOldBinary(path string) (string, error) {
	backup := path + ".old"
	// Best-effort: a stale backup only blocks us on Windows when locked.
	_ = os.Remove(backup)
	if err := os.Rename(path, backup); err == nil {
		return backup, nil
	}
	backup = fmt.Sprintf("%s.old.%d", path, time.Now().Unix())
	if err := os.Rename(path, backup); err != nil {
		return "", err
	}
	return backup, nil
}

// UpdateDesktopSibling refreshes the unarr-desktop binary installed NEXT TO
// the current executable, if any. Returns (false, nil) when no sibling exists
// (a CLI-only install is not an error) AND when the sibling's version marker
// shows it is already at `version` (skip — logged, nothing downloaded); true
// only when a new binary was actually installed. ErrNoDesktopAssets (release
// without signed desktop assets) is returned as-is so the caller can
// warn-and-continue without failing the CLI update that already succeeded.
func UpdateDesktopSibling(ctx context.Context, version string, logf func(string)) (bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("detect binary: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	sibling, ok := FindDesktopSibling(filepath.Dir(exe))
	if !ok {
		return false, nil
	}
	return UpdateDesktopBinary(ctx, sibling, version, logf)
}
