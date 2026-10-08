//go:build windows

package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"golang.org/x/sys/windows"
)

// Only uninstall needs to acknowledge an installation whose config parent has
// never existed. Never replace an existing lock or infer shutdown from a new
// inode. This preparation supports regular local NTFS ancestry; it cannot prove
// the history of a removed reparse alias. The real lock still gates cleanup.
func prepareMissingWindowsUninstallLockParent() error {
	parent := filepath.Dir(config.LockPath())
	if _, err := os.Lstat(parent); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect instance lock parent: %w", err)
	}
	if err := requireAbsentWindowsUninstallState(); err != nil {
		return err
	}
	if err := checkWindowsUninstallLockNamespace(parent); err != nil {
		return err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create absent instance lock parent: %w", err)
	}
	if info, err := os.Lstat(parent); err != nil {
		return fmt.Errorf("inspect created instance lock parent: %w", err)
	} else if !info.IsDir() {
		return errors.New("created instance lock parent is not a directory")
	}
	if err := checkWindowsUninstallLockNamespace(parent); err != nil {
		return err
	}
	return requireAbsentWindowsUninstallState()
}

func requireAbsentWindowsUninstallState() error {
	_, err := agent.LoadState()
	if errors.Is(err, agent.ErrDaemonNotRunning) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect daemon state before creating lock parent: %w", err)
	}
	// Present state is inconsistent with a genuinely absent lock namespace.
	// Neither an invalid PID nor an inconclusive process query proves shutdown.
	return errors.New("daemon state exists without its instance lock parent; refusing cleanup")
}

func checkWindowsUninstallLockNamespace(parent string) error {
	absolute, err := filepath.Abs(parent)
	if err != nil {
		return fmt.Errorf("resolve instance lock parent: %w", err)
	}
	volume := filepath.VolumeName(absolute)
	if len(volume) != 2 || volume[1] != ':' {
		return errors.New("absent instance lock parent requires a local fixed NTFS volume")
	}
	closest := ""
	for current := absolute; ; current = filepath.Dir(current) {
		exists, err := regularWindowsUninstallLockAncestor(current)
		if err != nil {
			return err
		}
		if exists && closest == "" {
			closest = current
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	if closest == "" {
		return errors.New("instance lock parent has no accessible local ancestor")
	}
	return requireLocalNTFSWindowsLockParent(closest)
}

func regularWindowsUninstallLockAncestor(parent string) (bool, error) {
	ptr, err := windows.UTF16PtrFromString(parent)
	if err != nil {
		return false, fmt.Errorf("resolve instance lock ancestor: %w", err)
	}
	attrs, err := windows.GetFileAttributes(ptr)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect instance lock ancestor %q: %w", parent, err)
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return false, fmt.Errorf("instance lock ancestor %q must be a regular directory without reparse points", parent)
	}
	return true, nil
}

func requireLocalNTFSWindowsLockParent(parent string) error {
	ptr, err := windows.UTF16PtrFromString(parent)
	if err != nil {
		return fmt.Errorf("resolve instance lock volume: %w", err)
	}
	var volume [32768]uint16
	if err := windows.GetVolumePathName(ptr, &volume[0], uint32(len(volume))); err != nil {
		return fmt.Errorf("inspect instance lock volume: %w", err)
	}
	if windows.GetDriveType(&volume[0]) != windows.DRIVE_FIXED {
		return errors.New("absent instance lock parent requires a local fixed NTFS volume")
	}
	var filesystem [256]uint16
	if err := windows.GetVolumeInformation(&volume[0], nil, 0, nil, nil, nil, &filesystem[0], uint32(len(filesystem))); err != nil {
		return fmt.Errorf("inspect instance lock filesystem: %w", err)
	}
	if !strings.EqualFold(windows.UTF16ToString(filesystem[:]), "NTFS") {
		return errors.New("absent instance lock parent requires a local fixed NTFS volume")
	}
	return nil
}
