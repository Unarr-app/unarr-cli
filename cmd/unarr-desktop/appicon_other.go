//go:build !linux

package main

// installAppIcon is a no-op outside Linux: macOS and Windows do not resolve app
// icons through an XDG icon theme. macOS takes its icon from the .app bundle
// (planned) and Windows from the executable's own resources, so neither has
// anything for a running process to install.
func installAppIcon() {}
