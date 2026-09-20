package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/charmbracelet/huh"
)

func configMount(cfg *config.Config) error {
	m := cfg.Mount
	fmt.Println("  Debrid accounts and Usenet credentials are managed on the Unarr website.")
	fmt.Println("  Remote mounting requires a paid plan. Upgrade: https://unarr.app/pricing")
	fmt.Println("  Setup: https://unarr.app/profile?tab=agents")
	if err := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Enable remote folder mounting?").Description("Optional and off by default. unarr prepares dependencies automatically and explains any required system permissions before installation.").Value(&m.Enabled),
		huh.NewInput().Title("Local NZB folder (optional)").Description("Uses the Usenet account configured on the web. Blank disables local NZB scanning.").Value(&m.NZBDir),
	)).Run(); err != nil {
		return err
	}
	m.CacheDir = expandHome(strings.TrimSpace(m.CacheDir))
	m.NZBDir = expandHome(strings.TrimSpace(m.NZBDir))
	if err := m.Validate(); err != nil {
		return err
	}
	if m.Enabled {
		ctx, cancel := mountContext(context.Background())
		defer cancel()
		if _, err := prepareMountDependencies(ctx, cfg); err != nil {
			return err
		}
		fmt.Println("Ready. Run unarr mount to use the default folder, or unarr mount <directory> to choose one.")
	}
	cfg.Mount = m
	return nil
}
