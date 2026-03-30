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
	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/arr"
	"github.com/torrentclaw/unarr/internal/config"
	"github.com/torrentclaw/unarr/internal/mediaserver"
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
	dim := color.New(color.FgHiBlack)

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

	apiKey := cfg.Auth.APIKey

	if apiKey == "" {
		// Try browser-based auth first (like Claude Code / GitHub CLI)
		fmt.Println("  Opening browser to connect your account...")
		fmt.Println()

		browserKey, browserErr := browserAuth(apiURL)
		if browserErr == nil && strings.HasPrefix(browserKey, "tc_") {
			apiKey = browserKey
			green.Println("  ✓ Connected via browser")
			fmt.Println()
		} else {
			// Fallback to manual API key entry
			if browserErr != nil {
				dim.Printf("  Could not connect automatically: %s\n", browserErr)
			}
			fmt.Println("  Paste your API key instead:")
			dim.Printf("  (get it from %s/profile?tab=apikey)\n", apiURL)
			fmt.Println()

			var err error
			err = huh.NewForm(
				huh.NewGroup(
					huh.NewInput().
						Title("Step 1/3 — API Key").
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
		}
	}

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

	// Detect media servers and library paths
	detected := mediaserver.Detect()
	if len(detected.Servers) > 0 {
		for _, s := range detected.Servers {
			cyan.Printf("  Detected %s at %s\n", s.Name, s.URL)
		}
		if len(detected.Paths) > 0 {
			dim.Printf("  Found media libraries: %s\n", strings.Join(detected.Paths, ", "))
		}
		fmt.Println()
	}

	// If no dir yet and we detected media paths, offer a Select; otherwise show Input
	needsInput := true
	if downloadDir == "" && len(detected.Paths) > 0 {
		var options []huh.Option[string]
		for _, p := range detected.Paths {
			options = append(options, huh.NewOption(p, p))
		}
		if parent := mediaserver.ParentDir(detected.Paths); parent != "" {
			options = append(options, huh.NewOption(parent+" (parent directory)", parent))
		}
		options = append(options, huh.NewOption(defaultDownloadDir()+" (default)", defaultDownloadDir()))
		options = append(options, huh.NewOption("Custom path...", "__custom__"))

		downloadDir = detected.Paths[0]
		err = huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Step 2/3 — Download Directory").
					Description("Detected media libraries on your system").
					Options(options...).
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
		needsInput = downloadDir == "__custom__"
		if needsInput {
			downloadDir = defaultDownloadDir()
		}
	}
	if downloadDir == "" {
		downloadDir = defaultDownloadDir()
	}
	if needsInput {
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

	// ── Debrid auto-detection from *arr ─────────────────────────────

	if resp.User.IsPro {
		debridTokens := detectDebridFromArr(dim)
		if len(debridTokens) > 0 {
			fmt.Println()
			cyan.Printf("  Found %d debrid token(s) from your *arr setup:\n", len(debridTokens))
			for _, dt := range debridTokens {
				masked := dt.Token
				if len(masked) > 8 {
					masked = masked[:8] + "..."
				}
				fmt.Printf("    %s (%s) — %s\n", dt.Provider, dt.Name, masked)
			}
			fmt.Println()

			var configureDebrid bool
			err = huh.NewForm(
				huh.NewGroup(
					huh.NewConfirm().
						Title("Configure debrid automatically?").
						Description("Validates and saves the token to your unarr account").
						Affirmative("Yes, configure").
						Negative("No, skip").
						Value(&configureDebrid),
				),
			).Run()
			if err == nil && configureDebrid {
				for _, dt := range debridTokens {
					fmt.Printf("  Configuring %s... ", dt.Provider)
					result, err := ac.ConfigureDebrid(context.Background(), agent.ConfigureDebridRequest{
						Provider: dt.Provider,
						Token:    dt.Token,
					})
					if err != nil {
						color.New(color.FgYellow).Printf("failed: %s\n", err)
					} else if result.Success {
						green.Printf("OK")
						if result.Account.Username != "" {
							fmt.Printf(" (%s", result.Account.Username)
							if result.Account.Premium {
								fmt.Print(", premium")
							}
							fmt.Print(")")
						}
						fmt.Println()
					} else if result.Error != "" {
						color.New(color.FgYellow).Printf("failed: %s\n", result.Error)
					}
				}
			}
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

// detectDebridFromArr does a lightweight scan for *arr instances and extracts
// debrid tokens from their download client configs.
func detectDebridFromArr(dim *color.Color) []arr.DebridToken {
	dim.Println("  Scanning for *arr instances with debrid...")

	instances := arr.Discover()
	if len(instances) == 0 {
		return nil
	}

	var tokens []arr.DebridToken
	for _, inst := range instances {
		if inst.App == "prowlarr" || inst.APIKey == "" {
			continue
		}
		client := arr.NewClient(inst.URL, inst.APIKey)
		dcs, _ := client.DownloadClients()
		if len(dcs) == 0 {
			continue
		}
		tokens = append(tokens, arr.ExtractDebridTokens(dcs, func(id int) []arr.Field {
			fields, _ := client.DownloadClientDetails(id)
			return fields
		})...)
	}
	return tokens
}
