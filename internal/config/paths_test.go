package config

import (
	"os"
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
	// Clear the custom env so XDG takes effect
	os.Unsetenv("UNARR_CONFIG_DIR")
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")

	dir := Dir()
	if dir != "/xdg/config/unarr" {
		t.Errorf("Dir() with XDG = %q, want /xdg/config/unarr", dir)
	}
}
