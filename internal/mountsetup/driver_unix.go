//go:build !windows

package mountsetup

import (
	"context"
	"fmt"
	"os"
	"runtime"
)

func runWindowsInstaller(context.Context, string) error {
	return fmt.Errorf("WinFsp installation requires Windows")
}

func driverReady() error {
	if runtime.GOOS == "darwin" {
		if info, err := os.Stat("/Library/Filesystems/macfuse.fs"); err == nil && info.IsDir() {
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
