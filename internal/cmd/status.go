package cmd

import (
	"fmt"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon status and active downloads",
		Long:  "Display the current state of the daemon, active downloads, and recent activity.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus()
		},
	}
}

func runStatus() error {
	bold := color.New(color.Bold)
	dim := color.New(color.FgHiBlack)

	fmt.Println()
	bold.Printf("  unarr %s\n", Version)
	fmt.Println()

	cfg := loadConfig()

	if cfg.Auth.APIKey == "" {
		dim.Println("  Not configured. Run 'unarr setup' first.")
		fmt.Println()
		return nil
	}

	fmt.Printf("  Agent:     %s (%s)\n", cfg.Agent.Name, cfg.Agent.ID[:8]+"...")
	fmt.Printf("  Downloads: %s\n", cfg.Download.Dir)
	fmt.Printf("  Method:    %s\n", cfg.Download.PreferredMethod)
	fmt.Println()

	dim.Println("  Daemon not running. Start with 'unarr daemon start'")
	dim.Println("  (Live status will be shown here when daemon is running)")
	fmt.Println()

	return nil
}
