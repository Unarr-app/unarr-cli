package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	tc "github.com/torrentclaw/go-client"

	"github.com/torrentclaw/unarr/internal/parser"
	"github.com/torrentclaw/unarr/internal/ui"
)

func newInspectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect <magnet|hash|name>",
		Short: "Inspect a torrent — TrueSpec analysis",
		Long: `Analyze a torrent by magnet URI, info hash, or name.

Parses the torrent metadata (quality, codec, language, year), queries unarr
for enriched data, and shows a detailed TrueSpec report including quality score,
seed health, and available alternatives.`,
		Example: `  unarr inspect "magnet:?xt=urn:btih:ABC123&dn=Movie.2023.1080p"
  unarr inspect abc123def456...  (40-char info hash)
  unarr inspect "Oppenheimer.2023.1080p.BluRay.x265"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			input := args[0]
			parsed := parser.Parse(input)

			client := getClient()
			ctx := context.Background()

			// Determine search query
			searchQuery := parsed.Name
			if searchQuery == "" && parsed.InfoHash != "" {
				searchQuery = parsed.InfoHash
			}
			if searchQuery == "" {
				return fmt.Errorf("could not extract a name or hash from input")
			}

			// Clean the name for searching
			cleanQuery := parser.ExtractSearchQuery(searchQuery)
			if cleanQuery == "" {
				cleanQuery = searchQuery
			}

			// Search for enriched data
			params := tc.SearchParams{
				Query:   cleanQuery,
				Quality: parsed.Quality,
			}
			resp, err := client.Search(ctx, params)
			if err != nil {
				return fmt.Errorf("search failed: %w", err)
			}

			// Find matching result
			if len(resp.Results) == 0 {
				if jsonOut {
					return json.NewEncoder(os.Stdout).Encode(map[string]any{
						"parsed": parsed,
						"found":  false,
					})
				}
				ui.PrintInspect(searchQuery, parsed.Year, nil, magnetURI(input, parsed))
				return nil
			}

			result := resp.Results[0]

			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"parsed":  parsed,
					"found":   true,
					"content": result,
				})
			}

			year := ui.FormatYear(result.Year)
			ui.PrintInspect(result.Title, year, result.Torrents, magnetURI(input, parsed))
			return nil
		},
	}

	return cmd
}

func magnetURI(input string, parsed parser.ParsedTorrent) string {
	if parsed.IsMagnet {
		return input
	}
	if parsed.InfoHash != "" {
		return fmt.Sprintf("magnet:?xt=urn:btih:%s", parsed.InfoHash)
	}
	return ""
}
