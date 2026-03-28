package cmd

import (
	"fmt"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

func newStubCmd(name, short string) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: short + " (coming soon)",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println()
			color.New(color.FgYellow).Printf("  ⚠️  '%s' is coming in a future release.\n", name)
			fmt.Println()
			fmt.Println("  Follow progress at: https://github.com/torrentclaw/torrentclaw-cli")
			fmt.Println()
		},
	}
}
