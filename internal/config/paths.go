package config

import (
	"os"
	"path/filepath"
	"runtime"
)

const appName = "unarr"

// Dir returns the configuration directory following XDG conventions.
//   - Linux:   ~/.config/unarr
//   - macOS:   ~/Library/Application Support/unarr
//   - Windows: %APPDATA%/unarr
//
// Overridable via UNARR_CONFIG_DIR env var.
func Dir() string {
	if d := os.Getenv("UNARR_CONFIG_DIR"); d != "" {
		return d
	}
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", appName)
	case "windows":
		return filepath.Join(os.Getenv("APPDATA"), appName)
	default: // linux, freebsd, etc.
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, appName)
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", appName)
	}
}

// FilePath returns the full path to the config file.
func FilePath() string {
	return filepath.Join(Dir(), "config.toml")
}

// LockPath returns the daemon single-instance lock file, alongside config.toml.
// Scoped to the config dir so a separate UNARR_CONFIG_DIR (e.g. a dev agent)
// gets its own lock and can run concurrently; two daemons sharing one config
// dir cannot — that's the case that causes cross-talk (same agentId/hash/secret
// racing each other).
func LockPath() string {
	return filepath.Join(Dir(), "unarr.lock")
}

// DataDir returns the data directory for logs, cache, etc.
//   - Linux:   ~/.local/share/unarr
//   - macOS:   ~/Library/Application Support/unarr
//   - Windows: %LOCALAPPDATA%/unarr
func DataDir() string {
	switch runtime.GOOS {
	case "darwin":
		return Dir() // macOS uses same dir for config and data
	case "windows":
		return filepath.Join(os.Getenv("LOCALAPPDATA"), appName)
	default:
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, appName)
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".local", "share", appName)
	}
}
