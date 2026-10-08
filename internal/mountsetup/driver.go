package mountsetup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var errDriverMissing = errors.New("mount driver is not installed")

func ensureDriver(ctx context.Context, opts Options) error {
	if err := prepareDriver(ctx, opts, driverReady, installDriver); err != nil {
		return err
	}
	return activateDriver(ctx, opts)
}

func prepareDriver(ctx context.Context, opts Options, ready func() error, install func(context.Context, Options) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := ready()
	if !errors.Is(err, errDriverMissing) {
		return err
	}
	explanation := driverExplanation(runtime.GOOS)
	if opts.Confirm == nil {
		return fmt.Errorf("%s\nRun unarr config mount in a terminal to approve installation", explanation)
	}
	if err := opts.Confirm(explanation); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := install(ctx, opts); err != nil {
		return err
	}
	if err := ready(); err != nil {
		return fmt.Errorf("driver is not ready: %w. Complete any system approval or requested restart, then run unarr mount again", err)
	}
	return nil
}

func driverExplanation(goos string) string {
	switch goos {
	case "windows":
		return "WinFsp is needed to make the remote library appear as a Windows drive or folder. unarr will download the verified official installer and install the default filesystem components automatically, showing progress. If a legacy WinFsp 1.x installation needs replacement, its official uninstaller must run first; close any other apps using WinFsp mounts before continuing. Windows will request administrator approval (UAC) if needed. Review WinFsp and its license at https://winfsp.dev before continuing. A restart may be required between removal and installation, but unarr will not restart your computer. Nothing will be installed until you agree."
	case "darwin":
		return "macFUSE is needed to make the remote library appear as a local folder on macOS. unarr will download the verified official macFUSE installer and install its filesystem driver. macOS will request your administrator password and may require approval in System Settings > Privacy & Security and a restart. On Apple Silicon, macOS may also require a security change in Recovery; unarr will not change that setting. Nothing will be installed until you agree."
	default:
		return linuxDriverExplanation()
	}
}

func linuxDriverExplanation() string {
	explanation := "FUSE is needed to make the remote library appear as a local folder on Linux. unarr will install the FUSE package and its dependencies using your system package manager, and load the fuse module if needed. sudo/doas may request your administrator password. This changes system packages; nothing will be installed until you agree."
	command, err := linuxPackageCommand(exec.LookPath)
	if err == nil && filepath.Base(command[0]) == "pacman" {
		explanation += " On Arch Linux this runs pacman -Syu: it also upgrades ALL installed system packages to avoid an unsupported partial upgrade. This may require a restart later. Cancel if you do not want a full system upgrade; unarr will not restart the computer."
	}
	return explanation
}

func installDriver(ctx context.Context, opts Options) error {
	if runtime.GOOS == "linux" {
		return installLinuxDriver(ctx)
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		return fmt.Errorf("automatic driver installation is unavailable on %s", runtime.GOOS)
	}
	// Elevated Windows Installer cannot reliably read the user's network share.
	// Stage system installers in the OS temp directory even if config/tools live
	// on a UNC path. rclone's private cache still follows the configured location.
	dir, err := os.MkdirTemp("", "unarr-mount-driver-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	a := winfspArtifact
	if runtime.GOOS == "darwin" {
		a = macfuseArtifact
	}
	fmt.Fprintln(opts.Output, "Downloading and verifying the official filesystem driver...")
	file, err := download(ctx, &http.Client{Timeout: 5 * time.Minute}, a, dir)
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		return installMacDriver(ctx, file, dir)
	}
	installer := filepath.Join(dir, "winfsp.msi")
	if err := os.Rename(file, installer); err != nil {
		return err
	}
	err = runWindowsInstaller(ctx, installer)
	var exit *exec.ExitError
	if errors.As(err, &exit) && (exit.ExitCode() == 3010 || exit.ExitCode() == 1641) {
		return fmt.Errorf("WinFsp setup requires a Windows restart before continuing. Restart, then run unarr mount again")
	}
	return err
}

func linuxPackageCommand(look func(string) (string, error)) ([]string, error) {
	for _, command := range [][]string{
		{"apt-get", "install", "-y", "fuse3"}, {"dnf", "install", "-y", "fuse3"},
		{"yum", "install", "-y", "fuse3"}, {"pacman", "-Syu", "--needed", "--noconfirm", "fuse3"},
		{"zypper", "--non-interactive", "install", "fuse3"}, {"apk", "add", "fuse3"},
	} {
		if path, err := look(command[0]); err == nil {
			return append([]string{path}, command[1:]...), nil
		}
	}
	return nil, fmt.Errorf("no supported package manager found; install FUSE using your distribution's package manager, then retry")
}

func privileged(ctx context.Context, command []string) error {
	if os.Geteuid() != 0 {
		for _, helper := range []string{"sudo", "doas"} {
			if path, err := exec.LookPath(helper); err == nil {
				return run(ctx, path, command...)
			}
		}
		return fmt.Errorf("administrator access is needed but sudo/doas is unavailable; ask the administrator to install FUSE")
	}
	return run(ctx, command[0], command[1:]...)
}

func installLinuxDriver(ctx context.Context) error {
	if !hasFuseHelper() {
		if err := installLinuxPackage(ctx); err != nil {
			return err
		}
	}
	if _, err := os.Stat("/dev/fuse"); errors.Is(err, os.ErrNotExist) {
		return privileged(ctx, []string{"modprobe", "fuse"})
	}
	return nil
}

func installLinuxPackage(ctx context.Context) error {
	command, err := linuxPackageCommand(exec.LookPath)
	if err != nil {
		return err
	}
	// Fresh Debian/Ubuntu images have no package index.
	if filepath.Base(command[0]) == "apt-get" {
		if err := privileged(ctx, []string{command[0], "update"}); err != nil {
			return err
		}
	}
	return privileged(ctx, command)
}

func hasFuseHelper() bool {
	for _, name := range []string{"fusermount3", "fusermount"} {
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
	}
	return false
}

func installMacDriver(ctx context.Context, file, dir string) error {
	volume := filepath.Join(dir, "volume")
	if err := os.Mkdir(volume, 0o700); err != nil {
		return err
	}
	if err := run(ctx, "hdiutil", "attach", "-nobrowse", "-readonly", "-mountpoint", volume, file); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := run(cleanup, "hdiutil", "detach", volume); err != nil {
			fmt.Fprintln(os.Stderr, "Could not eject macFUSE installer:", err)
		}
	}()
	packages, err := filepath.Glob(filepath.Join(volume, "*.pkg"))
	if err != nil || len(packages) != 1 {
		return fmt.Errorf("macFUSE installer package was not found in the verified image")
	}
	if err := privileged(ctx, []string{"/usr/sbin/installer", "-pkg", packages[0], "-target", "/"}); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "macFUSE installed. If macOS asks, approve it in System Settings > Privacy & Security and restart before mounting.")
	return nil
}

func isContainer() bool {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return strings.Contains(os.Getenv("container"), "podman")
}
