//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallAppIconWritesTheEmbeddedLogo(t *testing.T) {
	// Without this the .desktop launcher's Icon=unarr resolves to nothing and
	// the app menu entry is a generic placeholder.
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)

	installAppIcon()

	got, err := os.ReadFile(filepath.Join(home, appIconDir, appIconName+".png"))
	if err != nil {
		t.Fatalf("the icon was not installed: %v", err)
	}
	if !bytes.Equal(got, trayIcon) {
		// One source asset: the menu icon must be the tray icon, so they can
		// never drift apart.
		t.Error("the installed icon is not the embedded tray icon")
	}
}

func TestInstallAppIconLeavesAnExistingIconAlone(t *testing.T) {
	// It runs on every startup, so it must not rewrite what is already there —
	// a user who replaced the icon keeps their replacement.
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	path := filepath.Join(home, appIconDir, appIconName+".png")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	custom := []byte("not really a png, but the user's choice")
	if err := os.WriteFile(path, custom, 0o644); err != nil {
		t.Fatal(err)
	}

	installAppIcon()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, custom) {
		t.Error("an already-installed icon was overwritten")
	}
}

func TestInstallAppIconSurvivesAnUnwritableHome(t *testing.T) {
	// Best-effort: a home it cannot write to costs a pretty icon, never the
	// tray. This must return rather than panic or exit.
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("a file where a directory is needed"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", blocked)

	installAppIcon()
}

func TestDataHomeHonoursXDG(t *testing.T) {
	// The icon has to land under the same base the installer writes the
	// launcher into, or the theme never finds it.
	t.Setenv("XDG_DATA_HOME", "/custom/share")
	if got := dataHome(); got != "/custom/share" {
		t.Errorf("dataHome() = %q, want the XDG override", got)
	}

	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/home/someone")
	if got, want := dataHome(), "/home/someone/.local/share"; got != want {
		t.Errorf("dataHome() = %q, want %q", got, want)
	}
}
