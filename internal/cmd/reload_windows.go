//go:build windows

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/fatih/color"
	"github.com/torrentclaw/unarr/internal/agent"
)

// ReloadableConfig holds a reference to the daemon for hot-reload.
type ReloadableConfig struct {
	Daemon *agent.Daemon
}

// startReloadWatcher is a no-op on Windows (no SIGUSR1 support).
func startReloadWatcher(_ *ReloadableConfig) {}

// sendReloadSignal is not supported on Windows; instructs the user to restart instead.
func sendReloadSignal() error {
	fmt.Println()
	color.New(color.FgYellow).Println("  ⚠  Config reload via signal is not supported on Windows.")
	fmt.Println("  Use 'unarr daemon restart' to apply configuration changes.")
	fmt.Println()
	return nil
}

// killPID stops the daemon process on Windows using taskkill.
func killPID(pid int) error {
	cmd := exec.Command("taskkill", "/pid", strconv.Itoa(pid), "/f")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("stop daemon (PID %d): %w", pid, err)
	}
	color.New(color.FgGreen).Printf("  ✓ Daemon stopped (PID %d)\n", pid)
	fmt.Println()
	return nil
}
