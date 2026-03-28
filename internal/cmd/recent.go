package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	tc "github.com/torrentclaw/go-client"

	"github.com/torrentclaw/torrentclaw-cli/internal/ui"
)

func newRecentCmd() *cobra.Command {
	var (
		limit int
		page  int
	)

	cmd := &cobra.Command{
		Use:   "recent",
		Short: "Show recently added movies and TV shows",
		Long: `Display the most recently added movies and TV shows to the catalog.

Shows the latest additions ordered by ingestion date. Use --limit to
control how many results to show and --page for pagination.`,
		Example: `  unarr recent
  unarr recent --limit 20
  unarr recent --page 2 --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := getClient()

			resp, err := client.Recent(context.Background(), tc.RecentParams{Limit: limit, Page: page})
			if err != nil {
				return fmt.Errorf("failed to fetch recent content: %w", err)
			}

			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}

			ui.PrintRecentItems(resp.Items)
			return nil
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 10, "number of results")
	cmd.Flags().IntVar(&page, "page", 0, "page number")

	return cmd
}
