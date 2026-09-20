// Package mountsetup prepares optional mounting dependencies on demand. It never
// runs from the download daemon, and system changes require an explained opt-in.
package mountsetup

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
	"github.com/gofrs/flock"
)

type Options struct {
	Directory string
	Output    io.Writer
	// Confirm must display the supplied explanation before asking. Nil means
	// unattended: no privileged installation or OS permission prompt is allowed.
	Confirm func(explanation string) error
}

func Ensure(ctx context.Context, opts Options) (string, error) {
	if opts.Output == nil {
		opts.Output = io.Discard
	}
	if err := ensureDriver(ctx, opts); err != nil {
		return "", err
	}
	return ensureRclone(ctx, opts)
}

func mountCapable(ctx context.Context, path string) bool {
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := append([]string{"mount", "--help"}, MountFlags()...)
	cmd := exec.CommandContext(probe, path, args...)
	winproc.HideWindow(cmd)
	out, err := cmd.Output()
	// A macOS Homebrew build can exit successfully yet print only an
	// installation notice. Require the actual mount command's read-only flag.
	return err == nil && strings.Contains(string(out), "--read-only")
}

func ensureRclone(ctx context.Context, opts Options) (string, error) {
	a, err := rcloneArtifact(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	return ensureRcloneArtifact(ctx, opts, a, &http.Client{Timeout: 5 * time.Minute})
}

func ensureRcloneArtifact(ctx context.Context, opts Options, a artifact, client *http.Client) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if path, err := exec.LookPath("rclone"); err == nil && mountCapable(ctx, path) {
		return path, nil
	}
	dir := cacheDirectory(opts.Directory)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	lock := flock.New(filepath.Join(dir, "install.lock"))
	ok, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("rclone preparation interrupted")
	}
	defer func() { _ = lock.Unlock() }()
	dest := filepath.Join(dir, filepath.Base(a.Member))
	if info, err := os.Lstat(dest); err == nil && info.Mode().IsRegular() && mountCapable(ctx, dest) {
		return dest, nil
	}
	fmt.Fprintf(opts.Output, "Preparing rclone %s: it connects the remote library to your local folder. Downloading the verified official binary into %s; no administrator permission is needed.\n", rcloneVersion, dir)
	archive, err := download(ctx, client, a, dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(archive) }()
	if err := extractBinary(archive, a.Member, dest); err != nil {
		return "", err
	}
	if !mountCapable(ctx, dest) {
		return "", fmt.Errorf("rclone could not start with mount support; check your system's application permissions")
	}
	return dest, nil
}

func cacheDirectory(root string) string {
	return filepath.Join(root, "rclone-"+rcloneVersion+"-"+runtime.GOOS+"-"+runtime.GOARCH)
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	winproc.HideWindow(cmd)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}
