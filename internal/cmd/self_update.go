package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/torrentclaw-cli/internal/upgrade"
)

func newSelfUpdateCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Update unarr to the latest version",
		Long: `Download and install the latest version of unarr.

Checks GitHub for the latest release, verifies the checksum, and
replaces the current binary. A backup is kept at <binary>.backup.`,
		Example: `  unarr self-update
  unarr self-update --force`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSelfUpdate(force)
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "reinstall even if already up to date")

	return cmd
}

func runSelfUpdate(force bool) error {
	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	yellow := color.New(color.FgYellow)

	fmt.Println()
	bold.Println("  unarr self-update")
	fmt.Println()

	// Check latest version
	fmt.Print("  Checking latest version... ")
	ctx := context.Background()
	latest, err := upgrade.CheckLatest(ctx)
	if err != nil {
		fmt.Println()
		return fmt.Errorf("could not check latest version: %w", err)
	}

	currentClean := strings.TrimPrefix(Version, "v")
	fmt.Printf("v%s\n", latest)
	fmt.Printf("  Current version: v%s\n", currentClean)

	if currentClean == latest && !force {
		fmt.Println()
		green.Println("  ✓ Already up to date!")
		fmt.Println()
		return nil
	}

	if currentClean == latest && force {
		yellow.Println("  Forcing reinstall...")
	}

	fmt.Println()

	upgrader := &upgrade.Upgrader{
		CurrentVersion: currentClean,
		OnProgress: func(msg string) {
			fmt.Printf("  %s\n", msg)
		},
	}

	result := upgrader.Execute(ctx, latest)

	fmt.Println()
	if !result.Success {
		return fmt.Errorf("upgrade failed: %v", result.Error)
	}

	green.Printf("  ✓ Upgraded v%s → v%s\n", result.OldVersion, result.NewVersion)
	if result.BackupPath != "" {
		fmt.Printf("  Backup: %s\n", result.BackupPath)
	}
	fmt.Println()

	// If running as daemon, re-exec to restart with new binary
	// For interactive use, just suggest restarting
	if isRunningAsDaemon() {
		fmt.Println("  Restarting daemon with new version...")
		binPath, err := os.Executable()
		if err != nil {
			return fmt.Errorf("could not determine executable path: %w", err)
		}
		execErr := syscall.Exec(binPath, os.Args, os.Environ())
		if execErr != nil && runtime.GOOS == "windows" {
			// Windows doesn't support syscall.Exec — start new process
			proc := exec.Command(binPath, os.Args[1:]...)
			proc.Stdout = os.Stdout
			proc.Stderr = os.Stderr
			proc.Stdin = os.Stdin
			return proc.Start()
		}
		return execErr
	}

	return nil
}

func isRunningAsDaemon() bool {
	// Simple heuristic: check if "start" was in the original args
	for _, arg := range os.Args {
		if arg == "start" {
			return true
		}
	}
	return false
}
