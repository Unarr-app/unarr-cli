package cmd

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/torrentclaw-cli/internal/config"
)

var configCategories = []string{"downloads", "organization", "notifications", "device", "region", "connection", "advanced"}

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:       "config [category]",
		Short:     "Edit settings interactively",
		Long: `Edit unarr settings interactively with a category-based menu.

Categories:
  downloads        Download directory, method, speed limits, concurrency
  organization     Auto-sort into Movies / TV Shows folders
  notifications    Desktop notifications
  device           Agent name
  region           Country and language
  connection       API URL, API key
  advanced         Daemon poll & heartbeat intervals

Run without arguments to see the full menu, or specify a category
to jump directly to it.

Config file: ~/.config/unarr/config.toml
Environment variables override config file values:
  UNARR_API_KEY        API key
  UNARR_API_URL        API URL
  UNARR_COUNTRY        Default country code
  UNARR_DOWNLOAD_DIR   Download directory`,
		Example: `  unarr config               # Interactive menu
  unarr config downloads     # Jump to downloads settings
  unarr config region        # Jump to region settings`,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: configCategories,
		RunE: func(cmd *cobra.Command, args []string) error {
			category := ""
			if len(args) == 1 {
				category = args[0]
			}
			return runConfigMenu(category)
		},
	}

	return cmd
}

func runConfigMenu(category string) error {
	if !isTerminal() {
		return fmt.Errorf("interactive config requires a terminal (use UNARR_* env vars instead)")
	}

	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	dim := color.New(color.FgHiBlack)

	cfg := loadConfig()
	original := cfg // snapshot for change detection

	fmt.Println()
	bold.Println("  unarr config")
	fmt.Println()

	// Direct category access
	if category != "" {
		if err := runCategory(&cfg, category); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				fmt.Println("\n  Cancelled.")
				return nil
			}
			return err
		}
		return saveIfChanged(cfg, original, green, dim)
	}

	// Menu loop
	for {
		var choice string
		err := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Settings").
					Options(
						huh.NewOption("Downloads        — directory, method, speed limits", "downloads"),
						huh.NewOption("Organization     — auto-sort Movies & TV Shows", "organization"),
						huh.NewOption("Notifications    — desktop notifications", "notifications"),
						huh.NewOption("Device           — agent name", "device"),
						huh.NewOption("Region           — country & language", "region"),
						huh.NewOption("Connection       — API URL & key", "connection"),
						huh.NewOption("Advanced         — daemon intervals", "advanced"),
						huh.NewOption("Done             — save & exit", "done"),
					).
					Value(&choice),
			),
		).Run()
		if err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return saveIfChanged(cfg, original, green, dim)
			}
			return err
		}

		if choice == "done" {
			return saveIfChanged(cfg, original, green, dim)
		}

		if err := runCategory(&cfg, choice); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				continue // back to menu
			}
			return err
		}
	}
}

func runCategory(cfg *config.Config, category string) error {
	switch category {
	case "downloads":
		return configDownloads(cfg)
	case "organization":
		return configOrganization(cfg)
	case "notifications":
		return configNotifications(cfg)
	case "device":
		return configDevice(cfg)
	case "region":
		return configRegion(cfg)
	case "connection":
		return configConnection(cfg)
	case "advanced":
		return configAdvanced(cfg)
	default:
		return fmt.Errorf("unknown category %q — valid: %s", category, strings.Join(configCategories, ", "))
	}
}

func configDownloads(cfg *config.Config) error {
	concurrent := strconv.Itoa(cfg.Download.MaxConcurrent)
	validConcurrent := map[string]bool{"1": true, "2": true, "3": true, "4": true, "5": true, "6": true, "8": true, "10": true}
	if !validConcurrent[concurrent] {
		concurrent = "3"
	}

	validMethods := map[string]bool{"auto": true, "torrent": true, "debrid": true, "usenet": true}
	if !validMethods[cfg.Download.PreferredMethod] {
		cfg.Download.PreferredMethod = "auto"
	}

	validQualities := map[string]bool{"": true, "720p": true, "1080p": true, "2160p": true}
	if !validQualities[cfg.Download.PreferredQuality] {
		cfg.Download.PreferredQuality = ""
	}

	err := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Download directory").
				Value(&cfg.Download.Dir),
			huh.NewSelect[string]().
				Title("Preferred method").
				Options(
					huh.NewOption("Auto (torrent + debrid when available)", "auto"),
					huh.NewOption("Torrent only (BitTorrent P2P)", "torrent"),
					huh.NewOption("Debrid only (Real-Debrid, AllDebrid...)", "debrid"),
					huh.NewOption("Usenet only (requires Pro)", "usenet"),
				).
				Value(&cfg.Download.PreferredMethod),
			huh.NewSelect[string]().
				Title("Preferred quality").
				Description("Hint for automatic torrent selection").
				Options(
					huh.NewOption("Any (best available)", ""),
					huh.NewOption("720p", "720p"),
					huh.NewOption("1080p", "1080p"),
					huh.NewOption("2160p (4K)", "2160p"),
				).
				Value(&cfg.Download.PreferredQuality),
			huh.NewSelect[string]().
				Title("Max concurrent downloads").
				Options(
					huh.NewOption("1", "1"),
					huh.NewOption("2", "2"),
					huh.NewOption("3 (default)", "3"),
					huh.NewOption("4", "4"),
					huh.NewOption("5", "5"),
					huh.NewOption("6", "6"),
					huh.NewOption("8", "8"),
					huh.NewOption("10", "10"),
				).
				Value(&concurrent),
			huh.NewInput().
				Title("Max download speed").
				Description("0 = unlimited. Examples: 10MB, 500KB").
				Value(&cfg.Download.MaxDownloadSpeed).
				Validate(validateSpeed),
			huh.NewInput().
				Title("Max upload speed").
				Description("0 = unlimited. Examples: 1MB, 500KB").
				Value(&cfg.Download.MaxUploadSpeed).
				Validate(validateSpeed),
		),
	).Run()
	if err != nil {
		return err
	}

	cfg.Download.Dir = expandHome(strings.TrimSpace(cfg.Download.Dir))
	n, _ := strconv.Atoi(concurrent)
	if n > 0 {
		cfg.Download.MaxConcurrent = n
	}
	return nil
}

func configOrganization(cfg *config.Config) error {
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Auto-organize downloads?").
				Description("Sort files into Movies and TV Shows subdirectories").
				Value(&cfg.Organize.Enabled),
			huh.NewInput().
				Title("Movies directory").
				Value(&cfg.Organize.MoviesDir),
			huh.NewInput().
				Title("TV Shows directory").
				Value(&cfg.Organize.TVShowsDir),
		),
	).Run()
	if err != nil {
		return err
	}
	cfg.Organize.MoviesDir = expandHome(strings.TrimSpace(cfg.Organize.MoviesDir))
	cfg.Organize.TVShowsDir = expandHome(strings.TrimSpace(cfg.Organize.TVShowsDir))
	return nil
}

func configNotifications(cfg *config.Config) error {
	return huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Desktop notifications?").
				Description("Show a notification when a download completes").
				Value(&cfg.Notifications.Enabled),
		),
	).Run()
}

func configDevice(cfg *config.Config) error {
	dim := color.New(color.FgHiBlack)
	if cfg.Agent.ID != "" {
		dim.Printf("  Agent ID: %s\n\n", cfg.Agent.ID)
	}

	return huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Agent name").
				Description("Shown in the web dashboard").
				Value(&cfg.Agent.Name),
		),
	).Run()
}

func configRegion(cfg *config.Config) error {
	return huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Country").
				Description("ISO code for streaming providers (US, ES, DE, GB...)").
				Placeholder("US").
				Value(&cfg.General.Country),
			huh.NewInput().
				Title("Locale").
				Description("Language for content metadata (en, es, de, fr...)").
				Placeholder("en").
				Value(&cfg.General.Locale),
		),
	).Run()
}

func configConnection(cfg *config.Config) error {
	keyDesc := "Current: (none)"
	if k := cfg.Auth.APIKey; len(k) > 8 {
		keyDesc = "Current: " + k[:8] + "..."
	}

	return huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("API URL").
				Value(&cfg.Auth.APIURL),
			huh.NewInput().
				Title("API Key").
				Description(keyDesc).
				EchoMode(huh.EchoModePassword).
				Value(&cfg.Auth.APIKey),
		),
	).Run()
}

func configAdvanced(cfg *config.Config) error {
	return huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Poll interval").
				Description("How often to check for new tasks (e.g. 30s, 1m)").
				Value(&cfg.Daemon.PollInterval).
				Validate(validateDuration),
			huh.NewInput().
				Title("Heartbeat interval").
				Description("How often to send heartbeat to server (e.g. 30s, 1m)").
				Value(&cfg.Daemon.HeartbeatInterval).
				Validate(validateDuration),
		),
	).Run()
}

// ── Validators ──────────────────────────────────────────────────────

func validateSpeed(s string) error {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return nil
	}
	if _, err := config.ParseSpeed(s); err != nil {
		return fmt.Errorf("invalid speed: %s (e.g. 10MB, 500KB, 0)", s)
	}
	return nil
}

func validateDuration(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if _, err := time.ParseDuration(s); err != nil {
		return fmt.Errorf("invalid duration: %s (e.g. 30s, 1m, 5m)", s)
	}
	return nil
}

// ── Save logic ──────────────────────────────────────────────────────

func saveIfChanged(cfg, original config.Config, green, dim *color.Color) error {
	if reflect.DeepEqual(cfg, original) {
		dim.Println("  No changes made.")
		fmt.Println()
		return nil
	}

	if err := cfg.ValidatePaths(); err != nil {
		return fmt.Errorf("unsafe configuration: %w", err)
	}

	configPath := config.FilePath()
	if cfgFile != "" {
		configPath = cfgFile
	}

	if err := config.Save(cfg, configPath); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	appCfg = cfg // update cached config so subsequent calls see the new values

	fmt.Println()
	green.Printf("  ✓ Configuration saved to %s\n", configPath)
	fmt.Println()
	return nil
}
