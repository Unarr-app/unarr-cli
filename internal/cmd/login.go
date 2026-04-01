package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/fatih/color"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/config"
)

func newLoginCmd() *cobra.Command {
	var apiURL string

	cmd := &cobra.Command{
		Use:     "login",
		Aliases: []string{"auth"},
		Short:   "Authenticate with your torrentclaw account",
		Long: `Log in to your torrentclaw account by opening the browser or pasting
your API key manually. Use this when your API key has expired, been
revoked, or you want to switch to a different account.

Unlike 'unarr init', this command only updates your authentication
credentials — it does not modify your download directory, daemon
settings, or other configuration.`,
		Example: `  unarr login
  unarr login --api-url https://custom.server.com`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogin(apiURL)
		},
	}

	cmd.Flags().StringVar(&apiURL, "api-url", "", "API URL override (default: https://torrentclaw.com)")

	return cmd
}

func runLogin(apiURLOverride string) error {
	if !isTerminal() {
		return fmt.Errorf("interactive mode requires a terminal (use UNARR_API_KEY env var instead)")
	}

	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	dim := color.New(color.FgHiBlack)

	fmt.Println()
	bold.Println("  unarr login")
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

	// ── Authenticate ────────────────────────────────────────────────

	var apiKey string

	// Try browser-based auth first
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

		err := huh.NewForm(
			huh.NewGroup(
				huh.NewInput().
					Title("API Key").
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
				fmt.Println("\n  Login cancelled.")
				return nil
			}
			return err
		}
		apiKey = strings.TrimSpace(apiKey)
	}

	// ── Validate API key ────────────────────────────────────────────

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

	// ── Save config (auth fields only) ──────────────────────────────

	cfg.Auth.APIKey = apiKey
	cfg.Auth.APIURL = apiURL
	cfg.Agent.ID = agentID
	cfg.Agent.Name = agentName

	configPath := config.FilePath()
	if cfgFile != "" {
		configPath = cfgFile
	}

	if err := config.Save(cfg, configPath); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	appCfg = cfg

	fmt.Println()
	green.Println("  ✓ Credentials saved!")
	fmt.Printf("  Config: %s\n", configPath)
	fmt.Println()

	// Features summary
	if line := formatFeatures(resp.Features); line != "" {
		color.New(color.FgCyan).Printf("  Available:  %s\n", line)
		fmt.Println()
	}

	if cfg.Download.Dir == "" {
		fmt.Println("  Run " + bold.Sprint("unarr init") + " to complete the setup (download directory, daemon).")
		fmt.Println()
	}

	return nil
}
