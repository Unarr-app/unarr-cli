//go:build !windows

package mountsetup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

func runWindowsInstaller(context.Context, string) error {
	return fmt.Errorf("WinFsp installation requires Windows")
}

func activateDriver(ctx context.Context, opts Options) error {
	if runtime.GOOS != "darwin" || macDriverLoaded(ctx) {
		return nil
	}
	explanation := "macFUSE is installed but its filesystem extension is not active. unarr will ask macOS to load it so the local folder can work. macOS may require your password, approval in System Settings > Privacy & Security, and a restart. No package will be reinstalled and unarr will not restart your Mac."
	if opts.Confirm == nil {
		return fmt.Errorf("%s Run unarr mount in a terminal to continue", explanation)
	}
	if err := opts.Confirm(explanation); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	load, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := run(load, "/Library/Filesystems/macfuse.fs/Contents/Resources/load_macfuse")
	if err != nil || !macDriverLoaded(ctx) {
		return fmt.Errorf("macFUSE is installed but macOS has not activated it. Approve macFUSE in System Settings > Privacy & Security, complete any requested restart, then run unarr mount again (load result: %v)", err)
	}
	return nil
}

func macDriverLoaded(ctx context.Context) bool {
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probe, "/usr/sbin/kextstat", "-l")
	winproc.HideWindow(cmd)
	out, err := cmd.Output()
	return err == nil && strings.Contains(string(out), "io.macfuse.filesystems.macfuse")
}

func driverReady() error {
	if runtime.GOOS == "darwin" {
		if macDriverPresent("/Library/Filesystems/macfuse.fs") {
			return nil
		}
		return errDriverMissing
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("mount preparation is unsupported on %s", runtime.GOOS)
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		if isContainer() {
			return fmt.Errorf("FUSE is not exposed to this container. The host administrator must provide /dev/fuse and mount permissions; unarr cannot change the host from inside the container")
		}
		return errDriverMissing
	}
	f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("cannot access /dev/fuse: %w; ask the administrator to grant this user mount access", err)
	}
	_ = f.Close()
	if !hasFuseHelper() {
		return errDriverMissing
	}
	return nil
}

func macDriverPresent(root string) bool {
	// A leftover bundle directory after an interrupted uninstall is not a driver.
	for _, name := range []string{"mount_macfuse", "load_macfuse"} {
		info, err := os.Stat(filepath.Join(root, "Contents", "Resources", name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return false
		}
	}
	return true
}
