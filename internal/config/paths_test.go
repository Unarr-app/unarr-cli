package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDir(t *testing.T) {
	dir := Dir()
	if dir == "" {
		t.Error("Dir() returned empty string")
	}
	if !strings.Contains(dir, "unarr") {
		t.Errorf("Dir() = %q, should contain 'unarr'", dir)
	}
}

func TestFilePath(t *testing.T) {
	path := FilePath()
	if !strings.HasSuffix(path, "config.toml") {
		t.Errorf("FilePath() = %q, should end with config.toml", path)
	}
}

func TestLockPath(t *testing.T) {
	t.Setenv("UNARR_CONFIG_DIR", "/custom/path")
	path := LockPath()
	// filepath.Join, so the separator is native: `\custom\path\unarr.lock` on Windows.
	if want := filepath.Join("/custom/path", "unarr.lock"); path != want {
		t.Errorf("LockPath() = %q, want %q", path, want)
	}
}

func TestDataDir(t *testing.T) {
	dir := DataDir()
	if dir == "" {
		t.Error("DataDir() returned empty string")
	}
	if !strings.Contains(dir, "unarr") {
		t.Errorf("DataDir() = %q, should contain 'unarr'", dir)
	}
}

func TestDirOverrideEnv(t *testing.T) {
	t.Setenv("UNARR_CONFIG_DIR", "/custom/path")
	dir := Dir()
	if dir != "/custom/path" {
		t.Errorf("Dir() with env = %q, want /custom/path", dir)
	}
}

func TestDirXDGOverride(t *testing.T) {
	// XDG_CONFIG_HOME is a Linux/BSD convention. macOS and Windows deliberately
	// use ~/Library/Application Support and %APPDATA% instead — see Dir().
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("XDG_CONFIG_HOME does not apply on " + runtime.GOOS)
	}

	// Clear the custom env so XDG takes effect (restored by t.Setenv's cleanup).
	t.Setenv("UNARR_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")

	dir := Dir()
	if want := filepath.Join("/xdg/config", appName); dir != want {
		t.Errorf("Dir() with XDG = %q, want %q", dir, want)
	}
}
