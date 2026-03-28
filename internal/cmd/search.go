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

func newSearchCmd() *cobra.Command {
	var (
		contentType string
		quality     string
		lang        string
		genre       string
		yearMin     int
		yearMax     int
		minRating   float64
		sort        string
		limit       int
		page        int
		country     string
	)

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search for movies and TV shows",
		Long: `Search the catalog with advanced filters.

Results include torrent quality scores, seed health, and metadata from 30+ sources.`,
		Example: `  unarr search "breaking bad" --type show --quality 1080p
  unarr search "oppenheimer" --sort seeders --limit 5
  unarr search "inception" --lang es --min-rating 7
  unarr search "matrix" --json | jq '.results[].title'`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := getClient()

			params := tc.SearchParams{
				Query:     strings.Join(args, " "),
				Type:      contentType,
				Quality:   quality,
				Language:  lang,
				Genre:     genre,
				YearMin:   yearMin,
				YearMax:   yearMax,
				MinRating: minRating,
				Sort:      sort,
				Limit:     limit,
				Page:      page,
				Country:   country,
			}

			resp, err := client.Search(context.Background(), params)
			if err != nil {
				return fmt.Errorf("search failed: %w", err)
			}

			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}

			ui.PrintSearchResults(resp)
			return nil
		},
	}

	cmd.Flags().StringVar(&contentType, "type", "", "content type: movie, show")
	cmd.Flags().StringVar(&quality, "quality", "", "video quality: 480p, 720p, 1080p, 2160p")
	cmd.Flags().StringVar(&lang, "lang", "", "audio language (ISO 639 code, e.g. es, en)")
	cmd.Flags().StringVar(&genre, "genre", "", "genre filter (e.g. Action, Comedy, Drama)")
	cmd.Flags().IntVar(&yearMin, "year-min", 0, "minimum release year")
	cmd.Flags().IntVar(&yearMax, "year-max", 0, "maximum release year")
	cmd.Flags().Float64Var(&minRating, "min-rating", 0, "minimum IMDb/TMDb rating (0-10)")
	cmd.Flags().StringVar(&sort, "sort", "", "sort order: relevance, seeders, year, rating, added")
	cmd.Flags().IntVar(&limit, "limit", 0, "results per page (1-50)")
	cmd.Flags().IntVar(&page, "page", 0, "page number")
	cmd.Flags().StringVar(&country, "country", "", "country code for streaming availability (e.g. US, ES)")

	return cmd
}
