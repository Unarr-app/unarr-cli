//go:build windows

package main

// Windows autostart backend: the artifact is the "unarr-desktop" value under
// HKCU\Software\Microsoft\Windows\CurrentVersion\Run — per-user, so no
// elevation is ever required, and Explorer runs the value at login.

import (
	"errors"
	"os"

	"golang.org/x/sys/windows/registry"
)

const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

func autostartEnabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		// The Run key exists on any normal Windows install; treat a missing
		// key the same as a missing value — autostart is simply not enabled.
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer k.Close()
	if _, _, err := k.GetStringValue(autostartName); err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func setAutostart(enable bool) error {
	if !enable {
		k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
		if err != nil {
			if errors.Is(err, registry.ErrNotExist) {
				return nil // no Run key → nothing enabled; disable is idempotent
			}
			return err
		}
		defer k.Close()
		if err := k.DeleteValue(autostartName); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	// The running binary's own absolute path — the Run value must not depend
	// on PATH at login time.
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// CreateKey opens the key when it already exists (the normal case).
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(autostartName, registryRunValue(exe))
}
