package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/torrentclaw-cli/internal/agent"
	"github.com/torrentclaw/torrentclaw-cli/internal/arr"
	"github.com/torrentclaw/torrentclaw-cli/internal/config"
	"github.com/torrentclaw/torrentclaw-cli/internal/mediaserver"
)

func newMigrateCmd() *cobra.Command {
	var (
		dryRun     bool
		skipWanted bool
		radarrURL  string
		radarrKey  string
		sonarrURL  string
		sonarrKey  string
	)

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "[pre-beta] Import settings from Sonarr, Radarr, and Prowlarr",
		Long: `[PRE-BETA] This feature is under active development and may change.

Scans for existing *arr instances, imports your library preferences,
and queues downloads for wanted content — replacing your entire *arr stack.

Detects instances automatically via Docker, config files, and network scan.
You can also provide connection details manually with flags.

This command is read-only for your *arr apps — it only reads data,
never modifies them.

Config file: ~/.config/unarr/config.toml`,
		Example: `  unarr migrate                    # Auto-detect and migrate
  unarr migrate --dry-run           # Preview without applying changes
  unarr migrate --radarr-url http://localhost:7878 --radarr-key abc123`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrate(migrateOpts{
				DryRun:     dryRun,
				SkipWanted: skipWanted,
				RadarrURL:  radarrURL,
				RadarrKey:  radarrKey,
				SonarrURL:  sonarrURL,
				SonarrKey:  sonarrKey,
			})
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview changes without applying")
	cmd.Flags().BoolVar(&skipWanted, "skip-wanted", false, "don't import wanted list")
	cmd.Flags().StringVar(&radarrURL, "radarr-url", "", "Radarr URL (skip auto-detection)")
	cmd.Flags().StringVar(&radarrKey, "radarr-key", "", "Radarr API key")
	cmd.Flags().StringVar(&sonarrURL, "sonarr-url", "", "Sonarr URL (skip auto-detection)")
	cmd.Flags().StringVar(&sonarrKey, "sonarr-key", "", "Sonarr API key")

	return cmd
}

type migrateOpts struct {
	DryRun     bool
	SkipWanted bool
	RadarrURL  string
	RadarrKey  string
	SonarrURL  string
	SonarrKey  string
}

func runMigrate(opts migrateOpts) error {
	// JSON mode: skip interactive parts, text → stderr, JSON → stdout
	jsonMode := jsonOut && opts.DryRun
	if !jsonMode && !isTerminal() {
		return fmt.Errorf("interactive mode requires a terminal")
	}

	// In JSON mode, all progress text goes to stderr so stdout is clean JSON
	out := os.Stdout
	if jsonMode {
		out = os.Stderr
	}

	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	yellow := color.New(color.FgYellow)
	dim := color.New(color.FgHiBlack)
	cyan := color.New(color.FgCyan)

	// Point all color writers to the chosen output
	bold.SetWriter(out)
	green.SetWriter(out)
	yellow.SetWriter(out)
	dim.SetWriter(out)
	cyan.SetWriter(out)

	// Shorthand for writing to the output stream (not stdout in JSON mode)
	pr := func(format string, a ...any) { fmt.Fprintf(out, format, a...) }
	ln := func(a ...any) { fmt.Fprintln(out, a...) }

	cfg := loadConfig()

	// Check unarr is initialized
	if cfg.Auth.APIKey == "" {
		return fmt.Errorf("unarr is not configured yet — run 'unarr init' first")
	}

	ln()
	bold.Println("  unarr migrate")
	yellow.Println("  [pre-beta] This feature is under active development.")
	ln()

	// ── Phase 1: Discover instances ─────────────────────────────────

	instances := discoverInstances(opts, dim)

	if len(instances) == 0 {
		ln("  No *arr instances found automatically.")
		ln()

		// Offer manual entry
		manual, err := manualInstanceEntry()
		if err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				ln("\n  Migration cancelled.")
				return nil
			}
			return err
		}
		instances = manual
	}

	if len(instances) == 0 {
		ln("  No instances to migrate from. Exiting.")
		return nil
	}

	// Verify all instances and collect API keys where missing
	instances, err := verifyInstances(instances)
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			ln("\n  Migration cancelled.")
			return nil
		}
		return err
	}

	// ── Phase 2: Extract data ───────────────────────────────────────

	ln()
	dim.Println("  Fetching library data...")
	ln()

	var (
		movies          []arr.Movie
		series          []arr.Series
		radarrProfiles  []arr.QualityProfile
		sonarrProfiles  []arr.QualityProfile
		radarrFolders   []arr.RootFolder
		sonarrFolders   []arr.RootFolder
		indexers        []arr.Indexer
		downloadClients []arr.DownloadClient
		historyRecords  []arr.HistoryRecord
		blocklistItems  []arr.BlocklistItem
	)

	// First pass: discover extra instances from Prowlarr before fetching data
	urlSet := make(map[string]bool, len(instances))
	for _, inst := range instances {
		urlSet[strings.ToLower(inst.URL)] = true
	}

	var extraInstances []arr.Instance
	for _, inst := range instances {
		if inst.App != "prowlarr" {
			continue
		}
		client := arr.NewClient(inst.URL, inst.APIKey)
		if idx, err := client.Indexers(); err == nil {
			indexers = idx
		}
		extra := arr.DiscoverFromProwlarr(inst.URL, inst.APIKey)
		for _, e := range extra {
			key := strings.ToLower(e.URL)
			if !urlSet[key] {
				urlSet[key] = true
				extraInstances = append(extraInstances, e)
			}
		}
	}

	// Verify and append Prowlarr-discovered instances
	for i := range extraInstances {
		if err := arr.Verify(&extraInstances[i]); err == nil {
			instances = append(instances, extraInstances[i])
		}
	}

	// Second pass: fetch data from all Sonarr/Radarr instances
	for _, inst := range instances {
		client := arr.NewClient(inst.URL, inst.APIKey)

		switch inst.App {
		case "radarr":
			if m, err := client.Movies(); err == nil {
				movies = m
			} else {
				yellow.Printf("  Warning: could not fetch Radarr movies: %s\n", err)
			}
			if p, err := client.QualityProfiles(); err == nil {
				radarrProfiles = p
			}
			if f, err := client.RootFolders(); err == nil {
				radarrFolders = f
			}
			if d, err := client.DownloadClients(); err == nil {
				downloadClients = append(downloadClients, d...)
			}
			if h, err := client.History(250); err == nil {
				historyRecords = append(historyRecords, h...)
			}
			if b, err := client.Blocklist(250); err == nil {
				blocklistItems = append(blocklistItems, b...)
			}

		case "sonarr":
			if s, err := client.Series(); err == nil {
				series = s
			} else {
				yellow.Printf("  Warning: could not fetch Sonarr series: %s\n", err)
			}
			if p, err := client.QualityProfiles(); err == nil {
				sonarrProfiles = p
			}
			if f, err := client.RootFolders(); err == nil {
				sonarrFolders = f
			}
			if d, err := client.DownloadClients(); err == nil {
				downloadClients = append(downloadClients, d...)
			}
			if h, err := client.History(250); err == nil {
				historyRecords = append(historyRecords, h...)
			}
			if b, err := client.Blocklist(250); err == nil {
				blocklistItems = append(blocklistItems, b...)
			}
		}
	}

	result := arr.BuildMigrationResult(
		movies, series,
		radarrProfiles, sonarrProfiles,
		radarrFolders, sonarrFolders,
		indexers, downloadClients,
	)

	// Extract exclusion hashes from history and blocklist
	result.BlocklistedHashes = arr.ExtractBlocklistedHashes(blocklistItems)
	result.DownloadedHashes = arr.ExtractDownloadedHashes(historyRecords)

	// Extract debrid tokens from download clients (once, not per-instance)
	if len(downloadClients) > 0 {
		// Use the first available Sonarr/Radarr client for fetching field details
		var fieldsClient *arr.Client
		for _, inst := range instances {
			if inst.App != "prowlarr" && inst.APIKey != "" {
				fieldsClient = arr.NewClient(inst.URL, inst.APIKey)
				break
			}
		}
		if fieldsClient != nil {
			result.DebridTokens = arr.ExtractDebridTokens(downloadClients, func(id int) []arr.Field {
				fields, _ := fieldsClient.DownloadClientDetails(id)
				return fields
			})
		}
	}

	// Detect media servers
	detected := mediaserver.Detect()
	for _, s := range detected.Servers {
		result.MediaServers = append(result.MediaServers, fmt.Sprintf("%s at %s", s.Name, s.URL))
	}

	// ── Phase 3: Show instances table ───────────────────────────────

	green.Printf("  ✓ Found %d instance(s):\n", len(instances))
	ln()
	pr("    %-12s %-35s %-14s %s\n", "App", "URL", "Source", "Library")
	dim.Printf("    %-12s %-35s %-14s %s\n", "───", "───", "──────", "───────")

	for _, inst := range instances {
		lib := ""
		switch inst.App {
		case "radarr":
			wanted := len(result.WantedMovies)
			lib = fmt.Sprintf("%d movies (%d wanted)", result.TotalMovies, wanted)
		case "sonarr":
			wanted := len(result.WantedSeries)
			lib = fmt.Sprintf("%d series (%d wanted)", result.TotalSeries, wanted)
		case "prowlarr":
			lib = fmt.Sprintf("%d indexers", result.IndexerCount)
		}
		pr("    %-12s %-35s %-14s %s\n", inst.App, inst.URL, inst.Source, lib)
	}

	// ── Phase 4: Migration preview ──────────────────────────────────

	ln()
	ln("  ──────────────────────────────────────────────────────")
	ln()
	bold.Println("  Migration preview:")
	ln()

	// Config changes
	bold.Println("    Config:")
	if result.MoviesDir != "" {
		pr("      Movies directory     %-25s", result.MoviesDir)
		dim.Println(" (from Radarr root folder)")
	}
	if result.TVShowsDir != "" {
		pr("      TV Shows directory   %-25s", result.TVShowsDir)
		dim.Println(" (from Sonarr root folder)")
	}
	if result.Quality != "" {
		pr("      Preferred quality    %-25s", result.Quality)
		dim.Printf(" (from profile %q)\n", result.QualitySource)
	}
	if result.OrganizeEnabled {
		pr("      Auto-organize        %-25s\n", "enabled")
	}

	// Docker path warning
	if arr.HasDockerPaths(result) {
		ln()
		yellow.Println("    ⚠ These paths appear to be Docker container paths.")
		yellow.Println("      Your host paths may differ — verify after migration.")
	}

	// Wanted list
	totalWanted := len(result.WantedMovies) + len(result.WantedSeries)
	if totalWanted > 0 && !opts.SkipWanted {
		ln()
		bold.Printf("    Downloads to queue:    %d items\n", totalWanted)
		if len(result.WantedMovies) > 0 {
			pr("      %d movies", len(result.WantedMovies))
			dim.Println("            (monitored, not yet downloaded)")
		}
		if len(result.WantedSeries) > 0 {
			pr("      %d TV shows", len(result.WantedSeries))
			dim.Println("          (monitored, incomplete episodes)")
		}
	}

	// Exclusions
	totalExcluded := len(result.BlocklistedHashes) + len(result.DownloadedHashes)
	if totalExcluded > 0 {
		ln()
		bold.Println("    Exclusions:")
		if len(result.DownloadedHashes) > 0 {
			pr("      %d already downloaded", len(result.DownloadedHashes))
			dim.Println("    (from *arr history — won't re-download)")
		}
		if len(result.BlocklistedHashes) > 0 {
			pr("      %d blocklisted", len(result.BlocklistedHashes))
			dim.Println("          (rejected releases — will be skipped)")
		}
	}

	// Debrid tokens
	if len(result.DebridTokens) > 0 {
		ln()
		bold.Println("    Debrid tokens found:")
		for _, dt := range result.DebridTokens {
			masked := dt.Token
			if len(masked) > 8 {
				masked = masked[:8] + "..."
			}
			pr("      %s (%s) %s\n", dt.Provider, dt.Name, masked)
		}
		dim.Println("      Configure via: unarr config connection (or web dashboard)")
	}

	// Media servers
	if len(result.MediaServers) > 0 {
		ln()
		bold.Println("    Media servers detected:")
		for _, ms := range result.MediaServers {
			green.Printf("      ✓ %s\n", ms)
		}
		dim.Println("      These will keep working with your existing library.")
	}

	// Not needed anymore
	if result.IndexerCount > 0 || len(result.DownloadClients) > 0 {
		ln()
		bold.Println("    Not needed anymore:")
		if result.IndexerCount > 0 {
			pr("      %d indexers", result.IndexerCount)
			dim.Println("           (unarr searches 30+ sources automatically)")
		}
		if len(result.DownloadClients) > 0 {
			// Deduplicate client names
			seen := map[string]bool{}
			var names []string
			for _, n := range result.DownloadClients {
				if !seen[n] {
					seen[n] = true
					names = append(names, n)
				}
			}
			pr("      %s", strings.Join(names, ", "))
			dim.Println("  (unarr downloads directly via torrent/debrid/usenet)")
		}
	}

	ln()
	ln("  ──────────────────────────────────────────────────────")
	ln()

	// ── Phase 5: Confirm & apply ────────────────────────────────────

	if opts.DryRun {
		if jsonMode {
			// JSON export for scripting — write to real stdout
			jsonBytes, _ := json.MarshalIndent(result, "", "  ")
			_, _ = os.Stdout.Write(jsonBytes)
			_, _ = os.Stdout.Write([]byte("\n"))
		} else {
			cyan.Println("  Dry run — no changes applied.")
			ln()
		}
		return nil
	}

	var confirm bool
	err = huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Apply these changes?").
				Value(&confirm),
		),
	).Run()
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			ln("\n  Migration cancelled.")
			return nil
		}
		return err
	}
	if !confirm {
		dim.Println("  No changes applied.")
		ln()
		return nil
	}

	// Apply config changes (only overwrite if currently empty)
	changed := false
	if result.MoviesDir != "" && cfg.Organize.MoviesDir == "" {
		cfg.Organize.MoviesDir = result.MoviesDir
		changed = true
	}
	if result.TVShowsDir != "" && cfg.Organize.TVShowsDir == "" {
		cfg.Organize.TVShowsDir = result.TVShowsDir
		changed = true
	}
	if result.OrganizeEnabled && !cfg.Organize.Enabled {
		cfg.Organize.Enabled = true
		changed = true
	}
	if result.Quality != "" && cfg.Download.PreferredQuality == "" {
		cfg.Download.PreferredQuality = result.Quality
		changed = true
	}

	if changed {
		if err := cfg.ValidatePaths(); err != nil {
			return fmt.Errorf("unsafe configuration: %w", err)
		}

		configPath := configFilePath()
		if err := saveConfig(cfg, configPath); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		green.Println("  ✓ Configuration updated")
	}

	// Import wanted list
	if totalWanted > 0 && !opts.SkipWanted {
		allWanted := make([]arr.WantedItem, 0, len(result.WantedMovies)+len(result.WantedSeries))
		allWanted = append(allWanted, result.WantedMovies...)
		allWanted = append(allWanted, result.WantedSeries...)

		// Combine blocklisted + already-downloaded hashes to exclude
		excludeHashes := make([]string, 0, len(result.BlocklistedHashes)+len(result.DownloadedHashes))
		excludeHashes = append(excludeHashes, result.BlocklistedHashes...)
		excludeHashes = append(excludeHashes, result.DownloadedHashes...)

		if err := importWantedList(cfg, allWanted, excludeHashes, green, yellow, dim); err != nil {
			yellow.Printf("  Warning: could not queue downloads: %s\n", err)
			ln("  You can queue them manually from the web dashboard.")
		}
	}

	// ── Phase 6: Next steps ─────────────────────────────────────────

	ln()
	ln("  Your *arr apps are still running. When you're ready:")
	ln()
	ln("    1. Verify downloads are working:  " + bold.Sprint("unarr status"))
	ln("    2. Stop *arr services:            " + bold.Sprint("docker stop sonarr radarr prowlarr"))
	ln("    3. Keep your media server:        Plex / Jellyfin / Emby stays as-is")
	ln()

	return nil
}

// ── Discovery helpers ───────────────────────────────────────────────

func discoverInstances(opts migrateOpts, dim *color.Color) []arr.Instance {
	var instances []arr.Instance

	// Manual flags take priority
	hasManualFlags := opts.RadarrURL != "" || opts.SonarrURL != ""
	if hasManualFlags {
		if opts.RadarrURL != "" {
			instances = append(instances, arr.Instance{
				App:    "radarr",
				URL:    opts.RadarrURL,
				APIKey: opts.RadarrKey,
				Source: "manual",
			})
		}
		if opts.SonarrURL != "" {
			instances = append(instances, arr.Instance{
				App:    "sonarr",
				URL:    opts.SonarrURL,
				APIKey: opts.SonarrKey,
				Source: "manual",
			})
		}
		return instances
	}

	// Auto-discovery
	dim.Println("  Scanning for *arr instances...")
	return arr.Discover()
}

func verifyInstances(instances []arr.Instance) ([]arr.Instance, error) {
	var verified []arr.Instance
	for _, inst := range instances {
		if inst.APIKey == "" {
			// Ask user for API key
			var key string
			err := huh.NewForm(
				huh.NewGroup(
					huh.NewInput().
						Title(fmt.Sprintf("API key for %s (%s)", inst.App, inst.URL)).
						Description("Found via " + inst.Source + " but no API key available").
						Placeholder("Enter API key or leave empty to skip").
						Value(&key),
				),
			).Run()
			if err != nil {
				return nil, err
			}
			key = strings.TrimSpace(key)
			if key == "" {
				continue // skip this instance
			}
			inst.APIKey = key
		}

		if err := arr.Verify(&inst); err != nil {
			color.New(color.FgYellow).Printf("  Warning: %s at %s — %s (skipping)\n", inst.App, inst.URL, err)
			continue
		}
		verified = append(verified, inst)
	}
	return verified, nil
}

func manualInstanceEntry() ([]arr.Instance, error) {
	var radarrURL, radarrKey, sonarrURL, sonarrKey string

	err := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Radarr URL").
				Description("Leave empty to skip").
				Placeholder("http://localhost:7878").
				Value(&radarrURL),
			huh.NewInput().
				Title("Radarr API key").
				Value(&radarrKey),
			huh.NewInput().
				Title("Sonarr URL").
				Description("Leave empty to skip").
				Placeholder("http://localhost:8989").
				Value(&sonarrURL),
			huh.NewInput().
				Title("Sonarr API key").
				Value(&sonarrKey),
		),
	).Run()
	if err != nil {
		return nil, err
	}

	var instances []arr.Instance
	radarrURL = strings.TrimSpace(radarrURL)
	sonarrURL = strings.TrimSpace(sonarrURL)

	if radarrURL != "" && strings.TrimSpace(radarrKey) != "" {
		instances = append(instances, arr.Instance{
			App:    "radarr",
			URL:    radarrURL,
			APIKey: strings.TrimSpace(radarrKey),
			Source: "manual",
		})
	}
	if sonarrURL != "" && strings.TrimSpace(sonarrKey) != "" {
		instances = append(instances, arr.Instance{
			App:    "sonarr",
			URL:    sonarrURL,
			APIKey: strings.TrimSpace(sonarrKey),
			Source: "manual",
		})
	}
	return instances, nil
}

func importWantedList(cfg config.Config, items []arr.WantedItem, excludeHashes []string, green, yellow, dim *color.Color) error {
	apiURL := cfg.Auth.APIURL
	if apiURL == "" {
		apiURL = "https://torrentclaw.com"
	}

	ac := agent.NewClient(apiURL, cfg.Auth.APIKey, "unarr/"+Version)

	// Convert arr.WantedItem → agent.WantedItem
	agentItems := make([]agent.WantedItem, len(items))
	for i, item := range items {
		agentItems[i] = agent.WantedItem{
			TmdbID: item.TmdbID,
			ImdbID: item.ImdbID,
			Title:  item.Title,
			Year:   item.Year,
			Type:   item.Type,
		}
	}

	resp, err := ac.BatchDownload(context.Background(), agent.BatchDownloadRequest{
		Items:         agentItems,
		ExcludeHashes: excludeHashes,
	})
	if err != nil {
		return err
	}

	green.Printf("  ✓ %d downloads queued", resp.Queued)
	if resp.NotFound > 0 {
		fmt.Printf(" — %d not found in catalog", resp.NotFound)
	}
	if resp.AlreadyActive > 0 {
		fmt.Printf(" — %d already active", resp.AlreadyActive)
	}
	fmt.Println()

	if resp.Queued > 0 {
		dim.Println("    They'll start when the daemon runs.")
	}

	return nil
}

// configFilePath returns the config file path, respecting the --config flag.
func configFilePath() string {
	if cfgFile != "" {
		return cfgFile
	}
	return config.FilePath()
}

// saveConfig writes config to disk and updates the cached copy.
func saveConfig(cfg config.Config, path string) error {
	if err := config.Save(cfg, path); err != nil {
		return err
	}
	appCfg = cfg
	return nil
}
