//go:build linux

package main

// Installing the app-menu icon.
//
// The .desktop launcher names an icon ("unarr") that has to exist in the icon
// theme or the app shows up in the menu as a generic placeholder. The installer
// cannot drop it — it never has the image — but this binary already embeds the
// logo it puts in the tray, so it writes that same asset on startup. One source
// asset, so the menu icon can never drift from the tray icon, and existing
// installs are repaired without re-running the installer.

import (
	"fmt"
	"os"
	"path/filepath"
)

// appIconName is the Icon= value in the .desktop launcher.
const appIconName = "unarr"

// appIconDir is the hicolor path for the embedded logo's actual size (64x64).
// A wrong size directory is not a hard error for most themes, but it costs a
// rescale and can lose the icon in strict ones.
const appIconDir = "icons/hicolor/64x64/apps"

// installAppIcon writes the embedded logo into the user's icon theme unless it
// is already there. Best-effort throughout: a read-only or unusual home must
// cost the user a pretty icon, never the tray.
func installAppIcon() {
	dir := filepath.Join(dataHome(), appIconDir)
	path := filepath.Join(dir, appIconName+".png")
	if _, err := os.Stat(path); err == nil {
		return // already installed
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: app icon dir:", err)
		return
	}
	if err := os.WriteFile(path, trayIcon, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: app icon:", err)
	}
}

// dataHome is $XDG_DATA_HOME, falling back to the spec's ~/.local/share — the
// same base the installer writes the .desktop launcher into.
func dataHome() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".local/share"
	}
	return filepath.Join(home, ".local", "share")
}
