//go:build !windows

package main

// registerURLScheme is a no-op outside Windows: on Linux the unarr:// scheme
// is registered by install.sh --desktop (.desktop file with
// MimeType=x-scheme-handler/unarr; plus xdg-mime default + update-desktop-
// database) — a running process can't do it more reliably than the installer.
// On macOS a bare binary cannot own a URL scheme at all; that requires the
// future .app bundle's CFBundleURLTypes (player-handler plan, F2).
func registerURLScheme() {}
