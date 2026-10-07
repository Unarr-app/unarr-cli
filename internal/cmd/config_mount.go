package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/charmbracelet/huh"
)

func configMount(cfg *config.Config) error {
	m := cfg.Mount
	ctx, cancel := mountContext(context.Background())
	defer cancel()
	if err := printMountAccountStatus(ctx, cfg, os.Stdout); err != nil {
		return err
	}
	if err := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Enable remote folder mounting?").Description("Optional and off by default. unarr prepares dependencies automatically and explains any required system permissions before installation.").Value(&m.Enabled),
		huh.NewInput().Title("NZB inbox override (optional)").Description("Web-selected NZBs use a managed local inbox. Set a folder only to override its location.").Value(&m.NZBDir),
	)).Run(); err != nil {
		return err
	}
	m.CacheDir = expandHome(strings.TrimSpace(m.CacheDir))
	m.NZBDir = expandHome(strings.TrimSpace(m.NZBDir))
	if err := m.Validate(); err != nil {
		return err
	}
	if m.Enabled {
		fmt.Println("Settings prepared. Run unarr mount to validate the destination, set up dependencies and request background activation.")
	}
	cfg.Mount = m
	return nil
}

func printMountAccountStatus(ctx context.Context, cfg *config.Config, out io.Writer) error {
	fmt.Fprintln(out, "  Debrid accounts and Usenet credentials are managed on the Unarr website.")
	if err := checkMountAccount(ctx, cfg); err != nil {
		var httpError *agent.HTTPError
		if errors.As(err, &httpError) && httpError.StatusCode == http.StatusForbidden {
			fmt.Fprintln(out, "  Remote mounting requires a paid plan. Upgrade: https://unarr.app/pricing")
		}
		return err
	}
	fmt.Fprintln(out, "  Remote mounting is available because your account has an active paid plan.")
	fmt.Fprintln(out, "  Setup: https://unarr.app/profile?tab=agents")
	return nil
}
