//go:build linux

package main

// Linux autostart backend: the artifact is the XDG autostart entry
// ~/.config/autostart/unarr-desktop.desktop ($XDG_CONFIG_HOME honored by
// autostartDesktopPath). Desktop environments launch every entry in that
// directory at session login — no daemon or registration call involved.

import "os"

func autostartEnabled() (bool, error) {
	path, err := autostartDesktopPath()
	if err != nil {
		return false, err
	}
	return artifactExists(path)
}

func setAutostart(enable bool) error {
	path, err := autostartDesktopPath()
	if err != nil {
		return err
	}
	if !enable {
		return removeArtifact(path)
	}
	// The running binary's own absolute path, so the entry survives being
	// launched from a shell with a bare "unarr-desktop" on PATH.
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return writeArtifact(path, desktopFileContent(exe))
}
