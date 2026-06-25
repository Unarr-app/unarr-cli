package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/charmbracelet/huh"
	"github.com/fatih/color"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// clearRevokedIdentity wipes the stored credential (api key + agentId) after the
// server reports this machine's registration was revoked, so a re-run of the
// given command mints a fresh identity instead of looping against a dead key.
func clearRevokedIdentity(cfg config.Config, retryCmd string) {
	cfg.Auth.APIKey = ""
	cfg.Agent.ID = ""
	if err := config.Save(cfg, resolvedConfigPath()); err != nil {
		log.Printf("could not clear revoked credential: %v", err)
	}
	fmt.Println("  This machine's previous registration was removed from your account.")
	fmt.Printf("  Run `unarr %s` again to reconnect it as a new agent.\n", retryCmd)
	fmt.Println()
}

func newLoginCmd() *cobra.Command {
	var apiURL string

	cmd := &cobra.Command{
		Use:     "login",
		Aliases: []string{"auth"},
		Short:   "Authenticate with your unarr account",
		Long: `Log in to your unarr account by opening the browser or pasting
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

	// Resolve the agentId up front so the browser-authorize flow can bind the
	// minted per-machine key to it.
	agentID := cfg.Agent.ID
	if agentID == "" {
		agentID = uuid.New().String()
	}

	// Try browser-based auth first
	fmt.Println("  Opening browser to connect your account...")
	fmt.Println()

	browserKey, browserErr := browserAuth(apiURL, agentID)
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
		// The stored credential was revoked (this machine was deleted from the
		// dashboard). Drop it so the next run mints a fresh identity.
		if agent.IsRevoked(err) {
			clearRevokedIdentity(cfg, "login")
			return nil
		}
		return fmt.Errorf("API key validation failed: %w", err)
	}

	// Manual-paste bootstrap: the server minted a per-machine key bound to this
	// agentId. Swap to it and discard the general key the user pasted.
	if resp.AgentKey != "" {
		apiKey = resp.AgentKey
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
