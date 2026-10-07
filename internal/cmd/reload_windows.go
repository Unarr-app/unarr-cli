//go:build windows

package cmd

import (
	"fmt"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/fatih/color"
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

// Keep the platform signature used by the Unix stop branch, but never signal
// a Windows PID from a state file. Intent and supervisor shutdown come first.
func killPID(_ int) error {
	return stopDaemonByPID()
}
