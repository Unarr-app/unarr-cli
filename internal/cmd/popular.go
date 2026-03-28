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

func newPopularCmd() *cobra.Command {
	var (
		limit int
		page  int
	)

	cmd := &cobra.Command{
		Use:   "popular",
		Short: "Show popular content",
		Long:  "Display the most popular movies and TV shows, ranked by community engagement.",
		Example: `  unarr popular
  unarr popular --limit 20
  unarr popular --page 2 --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := getClient()

			resp, err := client.Popular(context.Background(), tc.PopularParams{Limit: limit, Page: page})
			if err != nil {
				return fmt.Errorf("failed to fetch popular content: %w", err)
			}

			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}

			ui.PrintPopularItems(resp.Items)
			return nil
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 10, "number of results")
	cmd.Flags().IntVar(&page, "page", 0, "page number")

	return cmd
}
