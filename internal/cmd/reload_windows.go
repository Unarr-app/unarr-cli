//go:build windows

package cmd

import "github.com/torrentclaw/unarr/internal/agent"

// ReloadableConfig holds a reference to the daemon for hot-reload.
type ReloadableConfig struct {
	Daemon *agent.Daemon
}

// startReloadWatcher is a no-op on Windows (no SIGUSR1 support).
func startReloadWatcher(_ *ReloadableConfig) {}
