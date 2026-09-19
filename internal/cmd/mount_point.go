package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func validateMountPoint(directory string) (string, error) {
	if runtime.GOOS == "windows" && len(directory) == 2 && directory[1] == ':' {
		letter := strings.ToUpper(directory[:1])
		if letter >= "A" && letter <= "Z" {
			return letter + ":", nil
		}
		return "", errors.New("invalid drive letter")
	}
	dir, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		return windowsMountDirectory(dir)
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
