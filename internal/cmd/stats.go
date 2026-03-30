package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/torrentclaw/unarr/internal/ui"
)

func newStatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show catalog statistics",
		Long: `Display aggregator statistics from the unarr catalog.

Shows total content count, torrent count, sources breakdown, and recent
ingestion activity. Useful for understanding the catalog coverage.`,
		Example: `  unarr stats
  unarr stats --json`,
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
