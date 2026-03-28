//go:build !windows

package cmd

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/torrentclaw/torrentclaw-cli/internal/agent"
	"github.com/torrentclaw/torrentclaw-cli/internal/config"
)

// ReloadableConfig holds a reference to the daemon for hot-reload.
type ReloadableConfig struct {
	Daemon *agent.Daemon
}

// startReloadWatcher listens for SIGUSR1 and reloads config.
// Only intervals are hot-reloadable (speeds require torrent client restart).
func startReloadWatcher(rc *ReloadableConfig) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1)

	go func() {
		for range sigCh {
			log.Println("Received SIGUSR1, reloading config...")

			cfg, err := config.Load("")
			if err != nil {
				log.Printf("Config reload failed: %v", err)
				continue
			}
			cfg.ApplyEnvOverrides()

			// Update poll interval
			if d, _ := time.ParseDuration(cfg.Daemon.PollInterval); d > 0 && rc.Daemon.PollTicker != nil {
				rc.Daemon.PollTicker.Reset(d)
				log.Printf("  Poll interval: %s", d)
			}

			// Update heartbeat interval
			if d, _ := time.ParseDuration(cfg.Daemon.HeartbeatInterval); d > 0 && rc.Daemon.HeartbeatTicker != nil {
				rc.Daemon.HeartbeatTicker.Reset(d)
				log.Printf("  Heartbeat interval: %s", d)
			}

			log.Println("Config reloaded successfully")
		}
	}()
}
