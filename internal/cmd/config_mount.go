package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/charmbracelet/huh"
)

func configMount(cfg *config.Config) error {
	m := cfg.Mount
	fmt.Println("  Debrid accounts and Usenet credentials are managed on the Unarr website.")
	fmt.Println("  Remote mounting requires a paid plan. Upgrade: https://unarr.app/pricing")
	fmt.Println("  Setup: https://unarr.app/profile?tab=agents")
	if err := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Enable remote folder mounting?").Description("Disabled by default. Start with unarr mount <directory>.").Value(&m.Enabled),
		huh.NewInput().Title("Loopback listener").Placeholder("127.0.0.1:11820").Value(&m.Listen),
		huh.NewInput().Title("Refresh interval").Placeholder("1m").Value(&m.RefreshInterval).Validate(validateDuration),
		huh.NewInput().Title("Metadata cache directory").Description("Blank uses the config directory.").Value(&m.CacheDir),
		huh.NewInput().Title("Local NZB folder (optional)").Description("Uses the Usenet account configured on the web. Blank disables local NZB scanning.").Value(&m.NZBDir),
	)).Run(); err != nil {
		return err
	}
	m.CacheDir = expandHome(strings.TrimSpace(m.CacheDir))
	if m.Enabled {
		if cfg.Auth.APIKey == "" {
			return fmt.Errorf("sign in to Unarr before enabling remote mounting")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := agent.NewClient(cfg.Auth.APIURL, cfg.Auth.APIKey, "unarr-mount").MountAccess(ctx); err != nil {
			return fmt.Errorf("mount access: %w", err)
		}
	}
	m.NZBDir = expandHome(strings.TrimSpace(m.NZBDir))
	if err := m.Validate(); err != nil {
		return err
	}
	cfg.Mount = m
	return nil
}
