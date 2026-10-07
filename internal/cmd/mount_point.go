package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

func validateMountPoint(directory string) (string, error) {
	if runtime.GOOS == "windows" {
		return windowsMountPoint(directory)
	}
	dir, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return "", errors.New("mount point must be an existing directory, not a symlink")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read mount point: %w", err)
	}
	if len(entries) != 0 {
		return "", errors.New("mount point must be empty")
	}
	return dir, nil
}

func windowsMountPoint(directory string) (string, error) {
	if config.IsWindowsMountDrive(directory) {
		return unusedWindowsDrive(strings.ToUpper(directory))
	}
	if len(directory) >= 2 && directory[1] == ':' && !filepath.IsAbs(directory) {
		return "", errors.New("mount point must be an unused drive letter or an absolute directory, not a drive-relative path")
	}
	dir, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	return windowsMountDirectory(dir)
}

func unusedWindowsDrive(drive string) (string, error) {
	if _, err := os.Stat(drive + `\`); !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("drive %s is occupied or unavailable; choose an unused drive letter", drive)
	}
	return drive, nil
}

func windowsMountDirectory(dir string) (string, error) {
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("on Windows the mount directory must not exist; use an unused drive letter or a new directory")
	}
	parent, err := os.Stat(filepath.Dir(dir))
	if err != nil || !parent.IsDir() {
		return "", errors.New("mount directory parent must exist")
	}
	return dir, nil
}
