package cmd

import (
	"github.com/spf13/cobra"
)

// newUpgradeCmd creates the `unarr upgrade` command as an alias for `self-update`.
func newUpgradeCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:     "upgrade",
		Aliases: []string{"update"},
		Short:   "Update unarr to the latest version",
		Long: `Download and install the latest version of unarr.

This is an alias for 'unarr self-update'. Checks GitHub for the latest
release, verifies the checksum, and replaces the current binary.
A backup is kept at <binary>.backup.`,
		Example: `  unarr upgrade
  unarr upgrade --force`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSelfUpdate(force)
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "reinstall even if already up to date")

	return cmd
}
