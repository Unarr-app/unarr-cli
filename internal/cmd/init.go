package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/fatih/color"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/torrentclaw-cli/internal/agent"
	"github.com/torrentclaw/torrentclaw-cli/internal/config"
)

func newInitCmd() *cobra.Command {
	var apiURL string

	cmd := &cobra.Command{
		Use:     "init",
		Aliases: []string{"setup"},
		Short:   "First-time configuration wizard",
		Long: `Interactive setup that connects your account, picks a download directory,
and optionally installs the daemon as a background service.

Opens your browser to create/copy your API key, validates it against the
server, and saves your configuration.

Run this once after installing unarr. To customize settings later,
use 'unarr config' or edit ~/.config/unarr/config.toml directly.`,
		Example: `  unarr init
  unarr init --api-url https://custom.server.com`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(apiURL)
		},
	}

	cmd.Flags().StringVar(&apiURL, "api-url", "", "API URL override (default: https://torrentclaw.com)")

	return cmd
}

func runInit(apiURLOverride string) error {
	if !isTerminal() {
		return fmt.Errorf("interactive mode requires a terminal (use UNARR_API_KEY env var instead)")
	}

	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	cyan := color.New(color.FgCyan)

	fmt.Println()
	bold.Println("  unarr init")
	fmt.Println()

	cfg := loadConfig()

	// Determine API URL
	apiURL := cfg.Auth.APIURL
	if apiURLOverride != "" {
		apiURL = apiURLOverride
	}
	if apiURL == "" {
		apiURL = "https://torrentclaw.com"
	}

	// ── Step 1/3: Connect account ───────────────────────────────────

	keysURL := apiURL + "/profile?tab=apikey"
	fmt.Printf("  Opening %s ...\n", keysURL)
	openBrowser(keysURL)
	fmt.Println()

	apiKey := cfg.Auth.APIKey
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Step 1/3 — API Key").
				Description("Copy it from the page that just opened in your browser").
				Placeholder("tc_...").
				Value(&apiKey).
				Validate(func(s string) error {
					s = strings.TrimSpace(s)
					if s == "" {
						return fmt.Errorf("API key is required")
					}
					if !strings.HasPrefix(s, "tc_") {
						return fmt.Errorf("API key should start with tc_")
					}
					return nil
				}),
		),
	).Run()
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			fmt.Println("\n  Init cancelled.")
			return nil
		}
		return err
	}
	apiKey = strings.TrimSpace(apiKey)

	// Validate API key by registering with the server
	fmt.Print("  Verifying API key... ")

	agentID := cfg.Agent.ID
	if agentID == "" {
		agentID = uuid.New().String()
	}

	hostname, _ := os.Hostname()
	agentName := cfg.Agent.Name
	if agentName == "" {
		agentName = hostname
	}

	ac := agent.NewClient(apiURL, apiKey, "unarr/"+Version)
	resp, err := ac.Register(context.Background(), agent.RegisterRequest{
		AgentID:     agentID,
		Name:        agentName,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Version:     Version,
		DownloadDir: cfg.Download.Dir,
	})
	if err != nil {
		color.Red("FAILED")
		fmt.Println()
		return fmt.Errorf("API key validation failed: %w", err)
	}

	green.Println("OK")
	fmt.Printf("  Connected as %s (%s) [%s]\n", resp.User.Name, resp.User.Email, strings.ToUpper(resp.User.Plan))
	fmt.Println()

	// ── Step 2/3: Download directory ────────────────────────────────

	downloadDir := cfg.Download.Dir
	if downloadDir == "" {
		downloadDir = defaultDownloadDir()
	}
	err = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Step 2/3 — Download Directory").
				Description("Where should downloaded files be saved?").
				Value(&downloadDir),
		),
	).Run()
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			fmt.Println("\n  Init cancelled.")
			return nil
		}
		return err
	}
	downloadDir = expandHome(strings.TrimSpace(downloadDir))

	// ── Step 3/3: Install daemon ────────────────────────────────────

	var installDaemon bool
	err = huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Step 3/3 — Install background service?").
				Description("Starts unarr automatically on boot (systemd/launchd)").
				Affirmative("Yes, install and start").
				Negative("No, I'll run it manually").
				Value(&installDaemon),
		),
	).Run()
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			fmt.Println("\n  Init cancelled.")
			return nil
		}
		return err
	}

	// ── Save config ─────────────────────────────────────────────────

	cfg.Auth.APIKey = apiKey
	cfg.Auth.APIURL = apiURL
	cfg.Agent.ID = agentID
	cfg.Agent.Name = agentName
	cfg.Download.Dir = downloadDir

	if cfg.Download.PreferredMethod == "" {
		cfg.Download.PreferredMethod = "auto"
	}

	if cfg.Organize.MoviesDir == "" {
		cfg.Organize.MoviesDir = filepath.Join(downloadDir, "Movies")
	}
	if cfg.Organize.TVShowsDir == "" {
		cfg.Organize.TVShowsDir = filepath.Join(downloadDir, "TV Shows")
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

	// ── Install daemon (if requested) ───────────────────────────────

	if installDaemon {
		fmt.Println()
		if err := runDaemonInstall(); err != nil {
			color.New(color.FgYellow).Printf("  Could not install daemon: %s\n", err)
			fmt.Println()
			fmt.Println("  You can install it later with: " + bold.Sprint("unarr daemon install"))
			fmt.Println("  Or run manually with:          " + bold.Sprint("unarr start"))
		}
	}

	// ── Summary ─────────────────────────────────────────────────────

	fmt.Println()
	green.Println("  ✓ unarr is ready!")
	fmt.Println()
	fmt.Printf("  Dashboard:  %s/downloads\n", apiURL)
	fmt.Printf("  Config:     %s\n", configPath)
	fmt.Println()

	// Features summary
	features := []string{}
	if resp.Features.Torrent {
		features = append(features, "Torrent")
	}
	if resp.Features.Debrid {
		features = append(features, "Debrid")
	}
	if resp.Features.Usenet {
		features = append(features, "Usenet")
	}
	if len(features) > 0 {
		cyan.Printf("  Available:  %s\n", strings.Join(features, ", "))
	}

	if !installDaemon {
		fmt.Println()
		fmt.Println("  Start the daemon:")
		fmt.Println("    " + bold.Sprint("unarr start") + "              foreground (Ctrl+C to stop)")
		fmt.Println("    " + bold.Sprint("unarr daemon install") + "     background service (auto-start on boot)")
	}

	fmt.Println()
	fmt.Println("  Customize speed limits, notifications, and more:")
	fmt.Println("    " + bold.Sprint("unarr config"))
	fmt.Println()

	return nil
}
