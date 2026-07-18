package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	tc "github.com/torrentclaw/go-client"
	"golang.org/x/term"

	"github.com/Unarr-app/unarr-cli/internal/ui"
)

func newSearchCmd() *cobra.Command {
	var (
		contentType   string
		quality       string
		lang          string
		genre         string
		yearMin       int
		yearMax       int
		minRating     float64
		sort          string
		limit         int
		page          int
		country       string
		stream        bool
		noInteractive bool
	)

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search for movies and TV shows",
		Long: `Search the catalog for movies and TV shows with advanced filters.

Results include torrent quality scores (0-100), seed health, resolution, codec,
audio, and metadata aggregated from 30+ sources.

On a terminal, results open an interactive picker: choose a title, choose a
release, then stream it, copy its magnet, or show its info hash. Use --stream to
jump straight to playback, --no-interactive for the static table, or --json for
machine-readable output that can be piped to jq or other tools.`,
		Example: `  unarr search "breaking bad" --type show --quality 1080p
  unarr search "oppenheimer" --sort seeders --stream
  unarr search "inception" --lang es --min-rating 7
  unarr search "matrix" --json | jq -r '.results[0].torrents[0].infoHash'`,
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

			// huh reads from stdin, so both stdout AND stdin must be a terminal
			// for the interactive picker; otherwise fall back to the table/auto-pick.
			interactive := !noInteractive &&
				term.IsTerminal(int(os.Stdout.Fd())) &&
				term.IsTerminal(int(os.Stdin.Fd()))

			// --stream on a non-interactive stdout (piped or --no-interactive):
			// auto-pick the best playable release and stream it.
			if stream && !interactive {
				tor, ok := firstStreamable(resp.Results)
				if !ok {
					return fmt.Errorf("no streamable release found for %q", strings.Join(args, " "))
				}
				return streamTorrent(tor)
			}

			if interactive {
				return runSearchInteractive(resp, stream)
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
	cmd.Flags().BoolVar(&stream, "stream", false, "pick a release and stream it (auto-picks the best on a non-interactive stdout)")
	cmd.Flags().BoolVar(&noInteractive, "no-interactive", false, "print the static results table instead of the interactive picker")

	// Shell completion for flags with known values
	cmd.RegisterFlagCompletionFunc("type", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"movie\tmovies only", "show\tTV shows only"}, cobra.ShellCompDirectiveNoFileComp
	})
	cmd.RegisterFlagCompletionFunc("quality", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"480p\tSD", "720p\tHD", "1080p\tFull HD", "2160p\t4K Ultra HD"}, cobra.ShellCompDirectiveNoFileComp
	})
	cmd.RegisterFlagCompletionFunc("sort", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"relevance\tbest match", "seeders\tmost seeders", "year\tnewest first", "rating\thighest rated", "added\trecently added"}, cobra.ShellCompDirectiveNoFileComp
	})
	cmd.RegisterFlagCompletionFunc("lang", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"en\tEnglish", "es\tSpanish", "fr\tFrench", "de\tGerman", "it\tItalian", "pt\tPortuguese", "ja\tJapanese", "ko\tKorean", "zh\tChinese", "ru\tRussian"}, cobra.ShellCompDirectiveNoFileComp
	})
	cmd.RegisterFlagCompletionFunc("genre", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"Action", "Adventure", "Animation", "Comedy", "Crime", "Documentary", "Drama", "Family", "Fantasy", "History", "Horror", "Music", "Mystery", "Romance", "Science Fiction", "Thriller", "War", "Western"}, cobra.ShellCompDirectiveNoFileComp
	})
	cmd.RegisterFlagCompletionFunc("country", completionCountryCodes)

	return cmd
}
