package cmd

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/sentry"
	"github.com/Unarr-app/unarr-cli/internal/upgrade"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
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
		Use:     "unarr",
		Version: Version,
		Short:   "Terminal torrent + debrid + usenet client — download, stream, transcode",
		Long: `unarr is a terminal-native client that downloads torrents, debrid links,
and usenet (NZB) — all from the same binary. It streams content straight
to mpv/vlc with sequential piece prioritization, transcodes on the fly via
ffmpeg with hardware acceleration (NVENC, QSV, VA-API, VideoToolbox), and
organizes your library into Movies/TV folders. Run it one-shot or as a
long-running daemon with a built-in WireGuard split-tunnel and remote
playback over Cloudflare Funnel.

Get started:
  unarr init                           First-time configuration wizard
  unarr download <magnet|hash>         Grab a torrent one-shot
  unarr start                          Start the download daemon

Documentation:  https://unarr.app/cli
Source:         https://github.com/Unarr-app/unarr-cli`,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if noColor || os.Getenv("NO_COLOR") != "" {
				color.NoColor = true
			}
			// Self-updater pulls signed releases primarily from the project's
			// public GitHub Releases, with the agent's API host (which proxies the
			// Hetzner mirror) as an automatic fallback — so a GitHub account
			// takedown can't strand the agent. UNARR_UPDATE_BASE overrides the
			// PRIMARY origin for staging/tests; empty keeps GitHub.
			cfg := loadConfig()
			upgrade.SetBaseURL(os.Getenv("UNARR_UPDATE_BASE"))
			upgrade.SetFallbackBaseURL(cfg.Auth.APIURL)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Command groups for organized help output
	rootCmd.AddGroup(
		&cobra.Group{ID: "start", Title: "Getting Started:"},
		&cobra.Group{ID: "search", Title: "Catalog & Discovery:"},
		&cobra.Group{ID: "download", Title: "Downloads & Streaming:"},
		&cobra.Group{ID: "daemon", Title: "Daemon Management:"},
		&cobra.Group{ID: "system", Title: "System & Diagnostics:"},
	)

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default ~/.config/unarr/config.toml)")
	rootCmd.PersistentFlags().StringVar(&apiKeyFlag, "api-key", "", "API key (overrides config file and env)")
	rootCmd.PersistentFlags().BoolVar(&jsonOut, "json", false, "output as JSON (for piping)")
	rootCmd.PersistentFlags().BoolVar(&noColor, "no-color", false, "disable colored output")

	// Getting Started
	initCmd := newInitCmd()
	initCmd.GroupID = "start"
	loginCmd := newLoginCmd()
	loginCmd.GroupID = "start"
	configCmd := newConfigCmd()
	configCmd.GroupID = "start"
	migrateCmd := newMigrateCmd()
	migrateCmd.GroupID = "start"

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
	upCmd := newUpCmd()
	upCmd.GroupID = "daemon"
	startCmd := newStartCmd()
	startCmd.GroupID = "daemon"
	stopCmd := newStopCmd()
	stopCmd.GroupID = "daemon"
	statusCmd := newStatusCmd()
	statusCmd.GroupID = "daemon"
	daemonCmd := newDaemonCmd()
	daemonCmd.GroupID = "daemon"
	vpnCmd := newVPNCmd()
	vpnCmd.GroupID = "daemon"
	funnelCmd := newFunnelCmd()
	funnelCmd.GroupID = "daemon"

	// System & Diagnostics
	statsCmd := newStatsCmd()
	statsCmd.GroupID = "system"
	doctorCmd := newDoctorCmd()
	doctorCmd.GroupID = "system"
	probeHWAccelCmd := newProbeHWAccelCmd()
	probeHWAccelCmd.GroupID = "system"
	cleanCmd := newCleanCmd()
	cleanCmd.GroupID = "system"
	mirrorsCmd := newMirrorsCmd()
	mirrorsCmd.GroupID = "system"
	selfUpdateCmd := newSelfUpdateCmd()
	selfUpdateCmd.GroupID = "system"
	versionCmd := newVersionCmd()
	versionCmd.GroupID = "system"
	completionCmd := newCompletionCmd()
	completionCmd.GroupID = "system"

	// Library
	scanCmd := newScanCmd()
	scanCmd.GroupID = "search"

	rootCmd.AddCommand(
		// Getting Started
		initCmd,
		loginCmd,
		configCmd,
		migrateCmd,
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
		upCmd,
		startCmd,
		stopCmd,
		statusCmd,
		daemonCmd,
		vpnCmd,
		funnelCmd,
		// System & Diagnostics
		statsCmd,
		doctorCmd,
		probeHWAccelCmd,
		cleanCmd,
		mirrorsCmd,
		selfUpdateCmd,
		versionCmd,
		completionCmd,
		// Library
		scanCmd,
		// Alias: upgrade → self-update
		newUpgradeCmd(),
	)
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		// Report to Sentry with command context
		command := ""
		if cmd, _, cerr := rootCmd.Find(os.Args[1:]); cerr == nil && cmd != nil && cmd != rootCmd {
			command = cmd.Name()
		}
		sentry.CaptureError(err, command)
		sentry.Close() // Flush before os.Exit (defers don't run after os.Exit)

		fmt.Fprintln(os.Stderr, color.RedString("Error: %s", err))
		os.Exit(1)
	}
}

// loadConfig loads config once (lazy initialization).
// resolvedConfigPath returns the config file the CLI actually reads/writes,
// honouring the global --config flag. Use this for every Save so a revocation
// wipe or key migration lands in the right file (e.g. the dev-local agent's
// ~/.config/unarr-dev/config.toml), not always the default path.
func resolvedConfigPath() string {
	if cfgFile != "" {
		return cfgFile
	}
	return config.FilePath()
}

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

	if appCfg.Agent.ID != "" {
		sentry.SetUser(appCfg.Agent.ID)
	}

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

	// Mirror failover for the public-API client, matching the agent control-plane
	// client's resilience: wrap the transport so search/popular/etc. rotate across
	// cfg.Auth.Mirrors on a primary takedown, using the same MirrorPool TYPE +
	// IsTransient policy the agent client uses (a fresh pool instance — the two
	// clients fail over independently). WithRetry(0) disables the go-client's own
	// retry loop so the transport owns failover exclusively (no nested
	// retry×backoff on an outage). WithTimeout(30s) is set idiomatically and gives
	// room for a couple of mirror attempts (go-client's bare default is 15s).
	pool := agent.NewMirrorPool(cfg.Auth.APIURL, cfg.Auth.Mirrors)
	opts = append(opts,
		tc.WithHTTPClient(&http.Client{Transport: agent.NewMirrorRoundTripper(pool, nil)}),
		tc.WithTimeout(30*time.Second),
		tc.WithRetry(0, 0, 0),
	)

	apiClient = tc.NewClient(opts...)
	return apiClient
}
