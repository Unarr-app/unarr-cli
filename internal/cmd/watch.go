package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	tc "github.com/torrentclaw/go-client"

	"github.com/torrentclaw/torrentclaw-cli/internal/ui"
)

func newWatchCmd() *cobra.Command {
	var country string

	cmd := &cobra.Command{
		Use:   "watch <query>",
		Short: "Find where to watch — streaming + torrents",
		Long: `Search for content and show streaming availability alongside torrent options.

Shows legal streaming options first (subscription, free, rent, buy),
then torrent alternatives below. Helps you decide the best way to watch.`,
		Example: `  unarr watch "oppenheimer"
  unarr watch "breaking bad" --country ES
  unarr watch "inception" --json`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := getClient()
			ctx := context.Background()

			if country == "" {
				country = loadConfig().General.Country
			}

			// Search for the content with country for streaming info
			resp, err := client.Search(ctx, tc.SearchParams{
				Query:   strings.Join(args, " "),
				Limit:   1,
				Country: country,
			})
			if err != nil {
				return fmt.Errorf("search failed: %w", err)
			}

			if len(resp.Results) == 0 {
				fmt.Println("No results found.")
				return nil
			}

			result := resp.Results[0]

			// Fetch watch providers
			providers, err := client.WatchProviders(ctx, result.ID, country)
			if err != nil {
				// Non-fatal: we can still show torrent results
				providers = nil
			}

			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"content":   result,
					"providers": providers,
				})
			}

			year := ui.FormatYear(result.Year)
			ui.PrintWatchProviders(result.Title, year, providers, result.Torrents)
			return nil
		},
	}

	cmd.Flags().StringVar(&country, "country", "", "country code for streaming availability (e.g. US, ES)")

	return cmd
}
