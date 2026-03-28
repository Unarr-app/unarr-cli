package ui

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	tc "github.com/torrentclaw/go-client"
)

var (
	titleColor   = color.New(color.FgCyan, color.Bold)
	headerColor  = color.New(color.FgWhite, color.Bold)
	successColor = color.New(color.FgGreen)
	warnColor    = color.New(color.FgYellow)
	errorColor   = color.New(color.FgRed)
	dimColor     = color.New(color.FgHiBlack)
	boldColor    = color.New(color.Bold)
)

// newCleanTable creates a borderless, minimal table with left-aligned columns.
func newCleanTable(w io.Writer) *tablewriter.Table {
	t := tablewriter.NewWriter(w)
	t.Configure(func(cfg *tablewriter.Config) {
		cfg.Header = tw.CellConfig{
			Alignment: tw.CellAlignment{Global: tw.AlignLeft},
			Padding:   tw.CellPadding{Global: tw.Padding{Left: " ", Right: " "}},
		}
		cfg.Row = tw.CellConfig{
			Alignment: tw.CellAlignment{Global: tw.AlignLeft},
			Padding:   tw.CellPadding{Global: tw.Padding{Left: " ", Right: " "}},
		}
	})
	return t
}

// PrintSearchResults renders search results as a colored table.
func PrintSearchResults(resp *tc.SearchResponse) {
	if len(resp.Results) == 0 {
		warnColor.Println("No results found.")
		return
	}

	fmt.Printf("\n")
	dimColor.Printf("  %d results found (page %d)\n\n", resp.Total, resp.Page)

	for _, r := range resp.Results {
		printSearchResultEntry(os.Stdout, r)
	}
}

func printSearchResultEntry(w io.Writer, r tc.SearchResult) {
	year := FormatYear(r.Year)
	titleColor.Fprintf(w, "  %s (%s)", r.Title, year)
	dimColor.Fprintf(w, "  [%s]", FormatContentType(r.ContentType))

	if r.RatingIMDb != nil {
		fmt.Fprintf(w, "  ⭐ %s", *r.RatingIMDb)
	}
	if len(r.Genres) > 0 {
		dimColor.Fprintf(w, "  %s", strings.Join(r.Genres, ", "))
	}
	fmt.Fprintln(w)

	if len(r.Torrents) == 0 {
		dimColor.Fprintln(w, "    No torrents available")
		fmt.Fprintln(w)
		return
	}

	table := newCleanTable(w)
	table.Header([]string{"", "Quality", "Size", "Seeds", "Source", "Codec", "Lang", "Score"})

	for _, t := range r.Torrents {
		quality := StringOrDash(t.Quality)
		size := FormatSize(t.SizeBytes)
		seeds := fmt.Sprintf("%s %d", SeedHealthIndicator(t.Seeders), t.Seeders)
		source := t.Source
		codec := StringOrDash(t.Codec)
		langs := FormatLanguages(t.Languages)
		score := ""
		if t.QualityScore != nil {
			score = fmt.Sprintf("%s %d", QualityIndicator(t.QualityScore), *t.QualityScore)
		}

		table.Append([]string{"   ", quality, size, seeds, source, codec, TruncateString(langs, 12), score})
	}

	table.Render()
	fmt.Fprintln(w)
}

// PrintPopularItems renders popular items as a colored table.
func PrintPopularItems(items []tc.PopularItem) {
	if len(items) == 0 {
		warnColor.Println("No popular items found.")
		return
	}

	fmt.Println()
	headerColor.Println("  🔥 Popular on unarr")
	fmt.Println()

	table := newCleanTable(os.Stdout)
	table.Header([]string{"#", "Title", "Year", "Type", "IMDb", "Seeds"})

	for i, item := range items {
		table.Append([]string{
			fmt.Sprintf("  %d", i+1),
			TruncateString(item.Title, 40),
			FormatYear(item.Year),
			FormatContentType(item.ContentType),
			FormatRating(item.RatingIMDb),
			FormatNumber(item.MaxSeeders),
		})
	}

	table.Render()
	fmt.Println()
}

// PrintRecentItems renders recent items as a colored table.
func PrintRecentItems(items []tc.RecentItem) {
	if len(items) == 0 {
		warnColor.Println("No recent items found.")
		return
	}

	fmt.Println()
	headerColor.Println("  🆕 Recently Added")
	fmt.Println()

	table := newCleanTable(os.Stdout)
	table.Header([]string{"#", "Title", "Year", "Type", "IMDb", "Added"})

	for i, item := range items {
		table.Append([]string{
			fmt.Sprintf("  %d", i+1),
			TruncateString(item.Title, 40),
			FormatYear(item.Year),
			FormatContentType(item.ContentType),
			FormatRating(item.RatingIMDb),
			FormatTimeAgo(item.CreatedAt),
		})
	}

	table.Render()
	fmt.Println()
}

// PrintStats renders system statistics.
func PrintStats(stats *tc.StatsResponse) {
	fmt.Println()
	headerColor.Println("  📊 unarr Statistics")
	fmt.Println()

	boldColor.Print("  Content:  ")
	fmt.Printf("%s movies, %s shows\n", FormatNumber(stats.Content.Movies), FormatNumber(stats.Content.Shows))

	boldColor.Print("  Enriched: ")
	fmt.Printf("%s with TMDb metadata\n", FormatNumber(stats.Content.TMDbEnriched))

	boldColor.Print("  Torrents: ")
	fmt.Printf("%s total, %s with seeders\n", FormatNumber(stats.Torrents.Total), FormatNumber(stats.Torrents.WithSeeders))

	if len(stats.Torrents.BySource) > 0 {
		fmt.Println()
		boldColor.Println("  Sources:")
		for source, count := range stats.Torrents.BySource {
			fmt.Printf("    %-20s %s\n", source, FormatNumber(count))
		}
	}

	if len(stats.RecentIngestions) > 0 {
		fmt.Println()
		boldColor.Println("  Recent Ingestions:")

		table := newCleanTable(os.Stdout)
		table.Header([]string{"", "Source", "Status", "Fetched", "New", "Updated"})

		for _, ing := range stats.RecentIngestions {
			status := ing.Status
			switch status {
			case "completed":
				status = successColor.Sprint("✓ done")
			case "running":
				status = warnColor.Sprint("⟳ running")
			case "failed":
				status = errorColor.Sprint("✗ failed")
			}
			table.Append([]string{
				"   ",
				ing.Source,
				status,
				FormatNumber(ing.Fetched),
				FormatNumber(ing.New),
				FormatNumber(ing.Updated),
			})
		}

		table.Render()
	}

	fmt.Println()
}

// PrintInspect renders the TrueSpec inspection output for a torrent.
func PrintInspect(title string, year string, torrents []tc.TorrentInfo, magnetURI string) {
	fmt.Println()
	titleColor.Printf("  📋 %s", title)
	if year != "" && year != "-" {
		titleColor.Printf(" (%s)", year)
	}
	fmt.Println()
	dimColor.Println("  " + strings.Repeat("─", len(title)+10))

	if len(torrents) == 0 {
		warnColor.Println("  No torrent details found.")
		fmt.Println()
		if magnetURI != "" {
			dimColor.Println("  Magnet:")
			fmt.Printf("  %s\n\n", magnetURI)
		}
		return
	}

	t := torrents[0]

	printField := func(label, value string) {
		boldColor.Printf("  %-12s", label+":")
		fmt.Println(value)
	}

	printField("Quality", StringOrDash(t.Quality)+" "+StringOrDash(t.SourceType))

	codecStr := StringOrDash(t.Codec)
	if t.AudioCodec != nil {
		codecStr += " / " + *t.AudioCodec
	}
	printField("Codec", codecStr)
	printField("Size", FormatSize(t.SizeBytes))
	printField("Seeds", fmt.Sprintf("%s %d  |  Leechers: %d", SeedHealthIndicator(t.Seeders), t.Seeders, t.Leechers))
	printField("Languages", FormatLanguages(t.Languages))
	printField("Source", t.Source)

	if t.QualityScore != nil {
		printField("Score", fmt.Sprintf("%s %d/100 (Quality Score)", QualityIndicator(t.QualityScore), *t.QualityScore))
	}

	printField("Health", fmt.Sprintf("%s (%s)", SeedHealthIndicator(t.Seeders), FormatSeedRatio(t.Seeders, t.Leechers)))

	if t.HDRType != nil {
		printField("HDR", *t.HDRType)
	}
	if t.ReleaseGroup != nil {
		printField("Group", *t.ReleaseGroup)
	}

	var flags []string
	if t.IsProper != nil && *t.IsProper {
		flags = append(flags, "PROPER")
	}
	if t.IsRepack != nil && *t.IsRepack {
		flags = append(flags, "REPACK")
	}
	if t.IsRemastered != nil && *t.IsRemastered {
		flags = append(flags, "REMASTERED")
	}
	if len(flags) > 0 {
		printField("Flags", strings.Join(flags, ", "))
	}

	fmt.Println()

	if len(torrents) > 1 {
		dimColor.Printf("  + %d more torrents available\n\n", len(torrents)-1)

		table := newCleanTable(os.Stdout)
		table.Header([]string{"", "Quality", "Size", "Seeds", "Source", "Score"})

		for i, tt := range torrents[1:] {
			score := ""
			if tt.QualityScore != nil {
				score = fmt.Sprintf("%s %d", QualityIndicator(tt.QualityScore), *tt.QualityScore)
			}
			table.Append([]string{
				fmt.Sprintf("  %d", i+2),
				StringOrDash(tt.Quality),
				FormatSize(tt.SizeBytes),
				fmt.Sprintf("%s %d", SeedHealthIndicator(tt.Seeders), tt.Seeders),
				tt.Source,
				score,
			})
		}

		table.Render()
		fmt.Println()
	}

	if magnetURI != "" {
		dimColor.Println("  Magnet:")
		fmt.Printf("  %s\n\n", magnetURI)
	}
}

// PrintWatchProviders renders streaming and torrent options.
func PrintWatchProviders(title string, year string, providers *tc.WatchProvidersResponse, torrents []tc.TorrentInfo) {
	fmt.Println()
	titleColor.Printf("  🎬 %s", title)
	if year != "" && year != "-" {
		titleColor.Printf(" (%s)", year)
	}
	fmt.Printf(" — Where to watch:\n\n")

	hasStreaming := false

	if providers != nil {
		if len(providers.Providers.Flatrate) > 0 {
			hasStreaming = true
			successColor.Println("  📺 SUBSCRIPTION (included):")
			for _, p := range providers.Providers.Flatrate {
				fmt.Printf("    • %s\n", p.Name)
			}
			fmt.Println()
		}

		if len(providers.Providers.Free) > 0 {
			hasStreaming = true
			successColor.Println("  🆓 FREE:")
			for _, p := range providers.Providers.Free {
				fmt.Printf("    • %s\n", p.Name)
			}
			fmt.Println()
		}

		if len(providers.Providers.Rent) > 0 {
			hasStreaming = true
			warnColor.Println("  💰 RENT:")
			for _, p := range providers.Providers.Rent {
				fmt.Printf("    • %s\n", p.Name)
			}
			fmt.Println()
		}

		if len(providers.Providers.Buy) > 0 {
			hasStreaming = true
			warnColor.Println("  🛒 BUY:")
			for _, p := range providers.Providers.Buy {
				fmt.Printf("    • %s\n", p.Name)
			}
			fmt.Println()
		}
	}

	if !hasStreaming {
		dimColor.Println("  📺 No streaming options found for your country.")
		fmt.Println()
	}

	if len(torrents) > 0 {
		headerColor.Println("  🏴‍☠️ TORRENT:")

		table := newCleanTable(os.Stdout)
		table.Header([]string{"", "Quality", "Size", "Seeds", "Source", "Score"})

		for _, t := range torrents {
			score := ""
			if t.QualityScore != nil {
				score = fmt.Sprintf("%s %d", QualityIndicator(t.QualityScore), *t.QualityScore)
			}
			table.Append([]string{
				"   ",
				StringOrDash(t.Quality),
				FormatSize(t.SizeBytes),
				fmt.Sprintf("%s %d", SeedHealthIndicator(t.Seeders), t.Seeders),
				t.Source,
				score,
			})
		}

		table.Render()
		fmt.Println()
	}

	if hasStreaming {
		successColor.Println("  💡 Available on streaming services above.")
	}
	fmt.Println()
}
