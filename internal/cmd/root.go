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
find streaming providers, and manage your media collection — all from your terminal.`,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if noColor || os.Getenv("NO_COLOR") != "" {
				color.NoColor = true
			}
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default ~/.config/unarr/config.toml)")
	rootCmd.PersistentFlags().StringVar(&apiKeyFlag, "api-key", "", "API key (overrides config file and env)")
	rootCmd.PersistentFlags().BoolVar(&jsonOut, "json", false, "output as JSON (for piping)")
	rootCmd.PersistentFlags().BoolVar(&noColor, "no-color", false, "disable colored output")

	rootCmd.AddCommand(
		newSetupCmd(),
		newStartCmd(),
		newStopCmd(),
		newDaemonCmd(),
		newDownloadCmd(),
		newStatusCmd(),
		newSearchCmd(),
		newInspectCmd(),
		newPopularCmd(),
		newRecentCmd(),
		newStatsCmd(),
		newWatchCmd(),
		newConfigCmd(),
		newDoctorCmd(),
		newVersionCmd(),
		// Stubs for future commands
		newStubCmd("upgrade", "Find a better version of a torrent"),
		newStubCmd("moreseed", "Find same quality with more seeders"),
		newStubCmd("compare", "Compare two torrents side by side"),
		newStubCmd("scan", "Scan your media library for upgrades"),
		newStreamCmd(),
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
