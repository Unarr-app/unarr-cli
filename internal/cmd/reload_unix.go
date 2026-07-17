//go:build !windows

package cmd

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/fatih/color"
)

// ReloadableConfig holds a reference to the daemon for hot-reload.
type ReloadableConfig struct {
	Daemon *agent.Daemon
}

// startReloadWatcher listens for SIGUSR1 and reloads config.
// With the sync-based architecture, intervals are fixed (3s watching, 60s idle).
//
// This used to Load() the config and THROW THE RESULT AWAY while logging "Config
// reloaded successfully" — so `unarr daemon reload` was a no-op that claimed to
// work. Users toggled allow_delete / preferred_method, reloaded, and the daemon
// kept reporting the startup values forever (the web then showed "file deletion
// not enabled" against a config.toml that plainly said true). Anything applied
// here MUST really be applied; anything that still needs a restart MUST say so.
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
			if rc == nil || rc.Daemon == nil {
				log.Println("Config reload: no daemon attached, nothing applied")
				continue
			}
			// ApplyReloadedConfig logs precisely what landed and what still needs
			// a restart — don't add a blanket "reloaded successfully" on top of it.
			rc.Daemon.ApplyReloadedConfig(cfg.Library.AllowDelete, cfg.Download.MethodOrder())
		}
	}()
}

// sendReloadSignal sends SIGUSR1 to the running daemon process.
func sendReloadSignal() error {
	state, err := agent.LoadState()
	if err != nil {
		if errors.Is(err, agent.ErrDaemonNotRunning) {
			return err
		}
		return fmt.Errorf("read daemon state: %w", err)
	}
	p, err := os.FindProcess(state.PID)
	if err != nil {
		return fmt.Errorf("find process %d: %w", state.PID, err)
	}
	if err := p.Signal(syscall.SIGUSR1); err != nil {
		return fmt.Errorf("send reload signal to PID %d: %w", state.PID, err)
	}
	fmt.Println()
	color.New(color.FgGreen).Printf("  ✓ Reload signal sent to daemon (PID %d)\n", state.PID)
	// Be precise about what a reload does and doesn't apply — claiming a blanket
	// "config re-read" is what sent users chasing a setting that never took.
	fmt.Println("  Applies now: allow_delete. Everything else needs 'unarr daemon restart'.")
	fmt.Println()
	return nil
}

// killPID sends SIGTERM to the given PID for a graceful shutdown.
func killPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("stop daemon (PID %d): %w", pid, err)
	}
	color.New(color.FgGreen).Printf("  ✓ Stop signal sent to daemon (PID %d)\n", pid)
	fmt.Println()
	return nil
}
