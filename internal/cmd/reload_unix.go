//go:build !windows

package cmd

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/config"
)

// ReloadableConfig holds a reference to the daemon for hot-reload.
type ReloadableConfig struct {
	Daemon *agent.Daemon
}

// startReloadWatcher listens for SIGUSR1 and reloads config.
// With the sync-based architecture, intervals are fixed (3s watching, 60s idle).
// Hot-reload now mainly serves as a signal to re-read config for future settings.
func startReloadWatcher(rc *ReloadableConfig) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1)

	go func() {
		for range sigCh {
			log.Println("Received SIGUSR1, reloading config...")

			_, err := config.Load("")
			if err != nil {
				log.Printf("Config reload failed: %v", err)
				continue
			}

			log.Println("Config reloaded successfully")
		}
	}()
}
