package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/upgrade"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

func newSelfUpdateCmd() *cobra.Command {
	var force bool
	var allowUnsigned bool

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
  unarr self-update --force
  unarr self-update --allow-unsigned   # accept releases missing checksums.txt.sig`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSelfUpdate(force, allowUnsigned)
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "reinstall even if already up to date")
	cmd.Flags().BoolVar(&allowUnsigned, "allow-unsigned", false, "continue with SHA256-only verification when checksums.txt.sig is missing")

	return cmd
}

func runSelfUpdate(force, allowUnsigned bool) error {
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
		// The CLI being current does NOT mean the desktop sibling is: the
		// common failure is the FIRST update after a release — desktop.yml
		// attaches its assets AFTER the release is published, so that run hits
		// ErrNoDesktopAssets (warn + skip) and the retry lands here, where an
		// early return used to strand the sibling on the old version forever.
		// The version marker makes this a cheap no-op when the sibling is
		// already current, so probing on every no-op update costs nothing.
		updateDesktopSibling(ctx, latest, green, yellow)
		fmt.Println()
		return nil
	}

	if currentClean == latest && force {
		yellow.Println("  Forcing reinstall...")
	}

	fmt.Println()

	upgrader := &upgrade.Upgrader{
		CurrentVersion: currentClean,
		AllowUnsigned:  allowUnsigned,
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

	updateDesktopSibling(ctx, latest, green, yellow)

	// Re-register the Windows autostart task so it adopts the new launcher.
	// `unarr update` only swaps the binary; an autostart task installed on an
	// older build still launches the daemon the OLD way (a hidden-PowerShell
	// wrapper / the console binary directly), so the boot console-window flash
	// the new wscript/VBS launcher fixes would persist until the user manually
	// re-ran `daemon install`. Best-effort: never fail an upgrade already on
	// disk, and a no-op when no task exists (nothing to adopt).
	if rewrote, err := reregisterWindowsTaskAfterUpgrade(); err != nil {
		yellow.Printf("  ! could not refresh the Windows startup task: %v\n", err)
		fmt.Println("    Re-run once to fix the boot startup: unarr daemon install")
	} else if rewrote {
		green.Println("  ✓ Windows startup task refreshed (no more console window at boot)")
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

// updateDesktopSibling refreshes the unarr-desktop tray companion installed
// NEXT TO this binary (same dir — never a PATH search) to the version the CLI
// just updated to. It runs only after a successful CLI upgrade: an old
// desktop binary can't be version-probed safely (pre-1.6 builds treat any
// argv as tray mode and would pop a tray window), so the sibling is refreshed
// unconditionally alongside each CLI upgrade instead. Failures here must
// never fail the CLI update that already succeeded — a release without signed
// desktop assets (desktop.yml attaches them AFTER the release is published)
// downgrades to a warning.
//
// The restart hint goes to stdout, not notify.Send: this command runs in a
// terminal, and we can't reliably detect a running tray cross-platform — an
// unconditional desktop notification would be noise on every headless update.
func updateDesktopSibling(ctx context.Context, version string, green, yellow *color.Color) {
	updated, err := upgrade.UpdateDesktopSibling(ctx, version, func(msg string) {
		fmt.Printf("  %s\n", msg)
	})
	switch {
	case errors.Is(err, upgrade.ErrNoDesktopAssets):
		yellow.Println("  ! unarr-desktop is installed next to unarr, but this release has no signed desktop assets — skipped")
	case err != nil:
		yellow.Printf("  ! unarr-desktop update failed: %v\n", err)
		fmt.Println("    (The unarr update itself succeeded. Retry later with: unarr-desktop --update)")
	case updated:
		green.Println("  ✓ unarr-desktop updated")
		fmt.Println("    If the unarr-desktop tray is running, quit and reopen it to load the new version.")
	}
}
