package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/upgrade"
)

func newSelfUpdateCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Update unarr to the latest version",
		Long: `Download and install the latest version of unarr.

Checks GitHub for the latest release, verifies the checksum, and
replaces the current binary. A backup is kept at <binary>.backup.

If the daemon is running, it is automatically restarted so the new
version is loaded into memory (otherwise heartbeat would keep
reporting the old version until a manual restart).`,
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
	red := color.New(color.FgRed)

	fmt.Println()
	bold.Println("  unarr self-update")
	fmt.Println()

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

	// Auto-restart daemon if it is running, otherwise the live process keeps
	// serving the old version (heartbeat reports old version → web gates
	// features against the wrong version).
	if state := agent.ReadState(); state != nil && isDaemonAlive(state) {
		fmt.Println()
		fmt.Printf("  → Daemon running (PID %d), restarting to load new version...\n", state.PID)
		if err := runDaemonSvcRestart(); err != nil {
			fmt.Println()
			red.Printf("  ✗ Auto-restart failed: %v\n", err)
			fmt.Println("    The new binary is on disk but the daemon is still running the old version.")
			fmt.Println("    Run manually: unarr daemon restart")
			fmt.Println("    (If the daemon runs under a different user/session, restart it there.)")
			fmt.Println()
			return nil
		}
		green.Println("  ✓ Daemon restarted")
	}

	fmt.Println()
	return nil
}
