// Package upgrade implements safe self-update for the unarr binary.
//
// The upgrade process:
//  1. Detect current binary path and verify write permissions
//  2. Download the release archive from GitHub
//  3. Verify SHA256 checksum against checksums.txt
//  4. Extract the binary from the archive
//  5. Smoke test: run the new binary with "version" to confirm it works
//  6. Backup the current binary
//  7. Replace with the new binary (preserving permissions)
//  8. On any failure: rollback from backup
package upgrade

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	binaryName  = "unarr"
	smokeTestTO = 5 * time.Second
)

// Result represents the outcome of an upgrade attempt.
type Result struct {
	Success    bool
	OldVersion string
	NewVersion string
	BackupPath string
	Error      error
}

// Upgrader handles downloading, verifying, and replacing the CLI binary.
type Upgrader struct {
	CurrentVersion string
	// OnProgress is called with status messages during the upgrade process.
	OnProgress func(msg string)
	// AllowUnsigned downgrades a missing checksums.txt.sig to a warning and
	// continues with SHA256-only verification. Required to downgrade to a
	// release published before signing was introduced, or to recover from
	// an accidental release where the workflow's signing step was skipped.
	// Default false — signature missing is a hard failure when a public
	// key is embedded.
	AllowUnsigned bool
}

func (u *Upgrader) log(msg string) {
	if u.OnProgress != nil {
		u.OnProgress(msg)
	}
	log.Printf("[upgrade] %s", msg)
}

// Execute performs a full upgrade to the target version.
func (u *Upgrader) Execute(ctx context.Context, targetVersion string) Result {
	targetVersion = strings.TrimPrefix(targetVersion, "v")

	if targetVersion == u.CurrentVersion {
		return Result{Success: true, OldVersion: u.CurrentVersion, NewVersion: targetVersion}
	}

	// 1. Detect current binary path
	binPath, err := os.Executable()
	if err != nil {
		return u.fail("detect binary: %v", err)
	}
	binPath, err = filepath.EvalSymlinks(binPath)
	if err != nil {
		return u.fail("resolve symlinks: %v", err)
	}

	// 2. Check Docker — self-update makes no sense in a container
	if isDocker() {
		return u.fail("running in Docker — update the container image instead")
	}

	// 3. Check write permissions
	binDir := filepath.Dir(binPath)
	if err := checkWritable(binDir); err != nil {
		return u.fail("no write permission to %s — run with elevated privileges or move the binary to a user-writable location", binDir)
	}

	// 4. Download archive
	u.log(fmt.Sprintf("Downloading v%s...", targetVersion))
	archivePath, err := downloadWithRetry(ctx, targetVersion, u.log)
	if err != nil {
		return u.fail("download: %v", err)
	}
	defer os.Remove(archivePath)

	// 5. Verify checksum (and signature, if configured)
	if SignatureVerificationConfigured() {
		u.log("Verifying checksum + ed25519 signature...")
	} else {
		u.log("Verifying checksum (release signature verification not configured for this build)...")
	}
	if err := verifyChecksum(ctx, targetVersion, archivePath); err != nil {
		if errors.Is(err, ErrMissingSignature) && u.AllowUnsigned {
			u.log("WARNING: release is unsigned and --allow-unsigned was passed; continuing with SHA256-only verification")
			if err := verifyChecksumOnly(ctx, targetVersion, archivePath); err != nil {
				return u.fail("checksum: %v", err)
			}
		} else {
			return u.fail("checksum: %v", err)
		}
	}

	// 6. Extract binary
	u.log("Extracting...")
	tmpDir, err := os.MkdirTemp("", "unarr-upgrade-*")
	if err != nil {
		return u.fail("create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	newBinPath, err := extractBinary(archivePath, tmpDir)
	if err != nil {
		return u.fail("extract: %v", err)
	}

	// 7. Smoke test
	u.log("Verifying new binary...")
	if err := smokeTest(newBinPath, targetVersion); err != nil {
		return u.fail("smoke test: %v", err)
	}

	// 8. Backup current binary
	backupPath := binPath + ".backup"
	u.log("Backing up current binary...")
	if err := os.Rename(binPath, backupPath); err != nil {
		return u.fail("backup: %v", err)
	}

	// 9. Replace with new binary
	u.log("Installing new binary...")
	if err := installBinary(newBinPath, binPath); err != nil {
		// Rollback
		u.log("Install failed, rolling back...")
		if rbErr := os.Rename(backupPath, binPath); rbErr != nil {
			return u.fail("install failed (%v) AND rollback failed (%v) — manual recovery needed at %s", err, rbErr, backupPath)
		}
		return u.fail("install (rolled back): %v", err)
	}

	u.log(fmt.Sprintf("Upgraded %s → %s", u.CurrentVersion, targetVersion))

	return Result{
		Success:    true,
		OldVersion: u.CurrentVersion,
		NewVersion: targetVersion,
		BackupPath: backupPath,
	}
}

func (u *Upgrader) fail(format string, args ...any) Result {
	err := fmt.Errorf(format, args...)
	u.log(fmt.Sprintf("FAILED: %v", err))
	return Result{
		Success:    false,
		OldVersion: u.CurrentVersion,
		Error:      err,
	}
}

// CheckLatest fetches the latest version from GitHub API and updates the cache.
func CheckLatest(ctx context.Context) (string, error) {
	v, err := fetchLatestVersion(ctx)
	if err == nil {
		writeCachedVersion(v)
	}
	return v, err
}

// installBinary copies the new binary to the target path, preserving original permissions.
func installBinary(src, dst string) error {
	// Read new binary
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read new binary: %w", err)
	}

	// Write to destination with executable permissions
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		return fmt.Errorf("write binary: %w", err)
	}

	return nil
}

// smokeTest runs the new binary with "version" and checks the output contains the expected version.
func smokeTest(binPath, expectedVersion string) error {
	ctx, cancel := context.WithTimeout(context.Background(), smokeTestTO)
	defer cancel()

	out, err := exec.CommandContext(ctx, binPath, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to run: %w (output: %s)", err, string(out))
	}

	output := string(out)
	if !strings.Contains(output, expectedVersion) {
		return fmt.Errorf("version mismatch: expected %q in output %q", expectedVersion, output)
	}

	return nil
}

// isDocker returns true if running inside a Docker container.
func isDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return false
}

// checkWritable verifies the directory is writable by creating and removing a temp file.
func checkWritable(dir string) error {
	tmp := filepath.Join(dir, ".unarr-write-test")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	f.Close()
	os.Remove(tmp)
	return nil
}

// archiveName returns the expected archive filename for this platform.
func archiveName(version string) string {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("%s_%s_%s_%s.%s", binaryName, version, runtime.GOOS, runtime.GOARCH, ext)
}

// githubOwnerRepo is the public GitHub repo the signed releases live in.
const githubOwnerRepo = "Unarr-app/unarr-cli"

// updateBaseURL is the asset host the self-updater downloads from. Default is
// the project's public GitHub Releases: GitHub serves release assets at
// `{base}/releases/download/v{ver}/{file}` — exactly the path releaseURL builds
// — so pointing the base at the repo is all it takes. SetBaseURL overrides it
// (UNARR_UPDATE_BASE for a staging origin, or an httptest.Server in tests); a
// custom base is assumed to mirror the same asset layout.
var updateBaseURL = "https://github.com/" + githubOwnerRepo

// githubAPIBase is the REST base for the version check. The list endpoint
// (releases?per_page=1) is used rather than releases/latest because the latter
// hides prereleases and the channel is currently all `-beta`. Only consulted
// while updateBaseURL still points at GitHub (isGitHubBase).
const githubAPIBase = "https://api.github.com/repos/" + githubOwnerRepo

// SetBaseURL overrides the release endpoint base (trailing slash trimmed).
// No-op for empty input so a blank override can't break the default.
func SetBaseURL(base string) {
	if base != "" {
		updateBaseURL = strings.TrimRight(base, "/")
	}
}

// isGitHubBase reports whether downloads still target GitHub (vs a staging or
// test origin set via SetBaseURL). Drives which version-check endpoint is used.
func isGitHubBase() bool {
	return strings.HasPrefix(updateBaseURL, "https://github.com/")
}

// releaseURL returns the download URL for a release asset:
//
//	{base}/releases/download/v{version}/{filename}
//
// which is GitHub's native release-asset path (and the layout a staging origin
// must mirror).
func releaseURL(version, filename string) string {
	return fmt.Sprintf("%s/releases/download/v%s/%s", updateBaseURL, version, filename)
}
