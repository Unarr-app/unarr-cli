package cmd

import (
	"fmt"
	"os"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/torrentclaw-cli/internal/config"
	tc "github.com/torrentclaw/go-client"
)

var (
	cfgFile    string
	apiKeyFlag string
	jsonOut    bool
	noColor    bool
	rootCmd    *cobra.Command
	apiClient  *tc.Client
	appCfg     config.Config
	cfgLoaded  bool
)

func init() {
	rootCmd = &cobra.Command{
		Use:   "unarr",
		Short: "unarr — torrent search and management",
		Long: `unarr is a powerful terminal tool for torrent search and management.

Search 30+ torrent sources, inspect torrent quality, discover popular content,
find streaming providers, and manage your media collection — all from your terminal.

Get started:
  unarr setup                          First-time configuration wizard
  unarr search "breaking bad"          Search for content
  unarr start                          Start the download daemon

Documentation:  https://torrentclaw.com/cli
Source:         https://github.com/torrentclaw/torrentclaw-cli`,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if noColor || os.Getenv("NO_COLOR") != "" {
				color.NoColor = true
			}
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Command groups for organized help output
	rootCmd.AddGroup(
		&cobra.Group{ID: "start", Title: "Getting Started:"},
		&cobra.Group{ID: "search", Title: "Search & Discovery:"},
		&cobra.Group{ID: "download", Title: "Downloads & Streaming:"},
		&cobra.Group{ID: "daemon", Title: "Daemon Management:"},
		&cobra.Group{ID: "system", Title: "System & Diagnostics:"},
	)

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default ~/.config/unarr/config.toml)")
	rootCmd.PersistentFlags().StringVar(&apiKeyFlag, "api-key", "", "API key (overrides config file and env)")
	rootCmd.PersistentFlags().BoolVar(&jsonOut, "json", false, "output as JSON (for piping)")
	rootCmd.PersistentFlags().BoolVar(&noColor, "no-color", false, "disable colored output")

	// Getting Started
	setupCmd := newSetupCmd()
	setupCmd.GroupID = "start"
	configCmd := newConfigCmd()
	configCmd.GroupID = "start"

	// Search & Discovery
	searchCmd := newSearchCmd()
	searchCmd.GroupID = "search"
	inspectCmd := newInspectCmd()
	inspectCmd.GroupID = "search"
	popularCmd := newPopularCmd()
	popularCmd.GroupID = "search"
	recentCmd := newRecentCmd()
	recentCmd.GroupID = "search"
	watchCmd := newWatchCmd()
	watchCmd.GroupID = "search"

	// Downloads & Streaming
	downloadCmd := newDownloadCmd()
	downloadCmd.GroupID = "download"
	streamCmd := newStreamCmd()
	streamCmd.GroupID = "download"

	// Daemon Management
	startCmd := newStartCmd()
	startCmd.GroupID = "daemon"
	stopCmd := newStopCmd()
	stopCmd.GroupID = "daemon"
	statusCmd := newStatusCmd()
	statusCmd.GroupID = "daemon"
	daemonCmd := newDaemonCmd()
	daemonCmd.GroupID = "daemon"

	// System & Diagnostics
	statsCmd := newStatsCmd()
	statsCmd.GroupID = "system"
	doctorCmd := newDoctorCmd()
	doctorCmd.GroupID = "system"
	selfUpdateCmd := newSelfUpdateCmd()
	selfUpdateCmd.GroupID = "system"
	versionCmd := newVersionCmd()
	versionCmd.GroupID = "system"
	completionCmd := newCompletionCmd()
	completionCmd.GroupID = "system"

	rootCmd.AddCommand(
		// Getting Started
		setupCmd,
		configCmd,
		// Search & Discovery
		searchCmd,
		inspectCmd,
		popularCmd,
		recentCmd,
		watchCmd,
		// Downloads & Streaming
		downloadCmd,
		streamCmd,
		// Daemon Management
		startCmd,
		stopCmd,
		statusCmd,
		daemonCmd,
		// System & Diagnostics
		statsCmd,
		doctorCmd,
		selfUpdateCmd,
		versionCmd,
		completionCmd,
		// Stubs for future commands
		newStubCmd("upgrade", "Find a better version of a torrent"),
		newStubCmd("moreseed", "Find same quality with more seeders"),
		newStubCmd("compare", "Compare two torrents side by side"),
		newStubCmd("scan", "Scan your media library for upgrades"),
		newStubCmd("add", "Search and add torrents to your client"),
		newStubCmd("monitor", "Watch for new episodes of a series"),
		newStubCmd("open", "Open content in the browser"),
	)
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, color.RedString("Error: %s", err))
		os.Exit(1)
	}
}

// loadConfig loads config once (lazy initialization).
func loadConfig() config.Config {
	if cfgLoaded {
		return appCfg
	}

	var err error
	appCfg, err = config.Load(cfgFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, color.YellowString("Warning: config load failed: %s", err))
		appCfg = config.Default()
	}

	appCfg.ApplyEnvOverrides()
	cfgLoaded = true
	return appCfg
}

// getClient returns a configured API client, initializing it on first use.
func getClient() *tc.Client {
	if apiClient != nil {
		return apiClient
	}

	cfg := loadConfig()

	var opts []tc.Option

	if cfg.Auth.APIURL != "" {
		opts = append(opts, tc.WithBaseURL(cfg.Auth.APIURL))
	}

	apiKey := apiKeyFlag
	if apiKey == "" {
		apiKey = cfg.Auth.APIKey
	}
	if apiKey != "" {
		opts = append(opts, tc.WithAPIKey(apiKey))
	}

	opts = append(opts, tc.WithUserAgent("unarr/"+Version))

	apiClient = tc.NewClient(opts...)
	return apiClient
}
