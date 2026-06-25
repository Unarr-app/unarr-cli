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
	fmt.Println("  Config will be re-read shortly.")
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
