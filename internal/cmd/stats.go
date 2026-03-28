package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/torrentclaw/torrentclaw-cli/internal/ui"
)

func newStatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "stats",
		Short:   "Show system statistics",
		Long:    "Display aggregator statistics including content counts, torrent sources, and recent ingestion history.",
		Example: `  unarr stats`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := getClient()

			resp, err := client.Stats(context.Background())
			if err != nil {
				return fmt.Errorf("failed to fetch stats: %w", err)
			}

			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}

			ui.PrintStats(resp)
			return nil
		},
	}

	return cmd
}
