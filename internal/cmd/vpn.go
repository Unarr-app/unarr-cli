package cmd

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/config"
	"github.com/torrentclaw/unarr/internal/vpn"
)

func newVPNCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vpn",
		Short: "Manage the managed-VPN split-tunnel for downloads",
		Long: `Enable, disable, and inspect the managed VPN.

When enabled, the daemon fetches a WireGuard config from your TorrentClaw account
at startup and routes ONLY the torrent client's traffic (peers + trackers) through
an in-process WireGuard tunnel — no root, no OS routing changes.

This is split-tunnel: your browser and other apps keep using your real IP. Only
your downloads are hidden behind the VPN server.

The VPN requires a PRO+ plan with the VPN add-on. Set it up at
https://torrentclaw.com/vpn and configure your other devices (phone, laptop) with
the OpenVPN credentials from your profile — those don't share the agent's tunnel.`,
		Example: `  unarr vpn status      # is the tunnel up? which server?
  unarr vpn enable      # turn the managed VPN on
  unarr vpn disable     # turn it off`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newVPNStatusCmd(), newVPNEnableCmd(), newVPNDisableCmd())
	return cmd
}

func newVPNStatusCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:     "status",
		Short:   "Show VPN configuration and live tunnel state",
		Example: "  unarr vpn status\n  unarr vpn status --check   # also verify your account is provisioned",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVPNStatus(check)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "query the API to verify the VPN is provisioned on your account")
	return cmd
}

func runVPNStatus(check bool) error {
	bold := color.New(color.Bold)
	dim := color.New(color.FgHiBlack)
	green := color.New(color.FgGreen)
	yellow := color.New(color.FgYellow)
	cyan := color.New(color.FgCyan)

	cfg := loadConfig()

	fmt.Println()
	bold.Println("  Managed VPN")
	fmt.Println()

	// ── Configured mode ──
	switch {
	case cfg.Download.VPN.ConfigFile != "":
		cyan.Println("  Mode:    self-hosted (local config_file)")
		fmt.Printf("  Config:  %s\n", cfg.Download.VPN.ConfigFile)
	case cfg.Download.VPN.Enabled:
		cyan.Println("  Mode:    managed (config fetched from your account)")
	default:
		dim.Println("  Mode:    off")
		fmt.Println()
		dim.Println("  Enable with `unarr vpn enable` (needs a PRO+ plan with the VPN add-on).")
		fmt.Println()
		return nil
	}

	// ── Live tunnel state (from the daemon state file) ──
	state := agent.ReadState()
	alive := state != nil && isDaemonAlive(state)
	fmt.Println()
	switch {
	case alive && state.VPNActive:
		server := state.VPNServer
		if host, _, err := net.SplitHostPort(server); err == nil && host != "" {
			server = host
		}
		green.Println("  ✓ Tunnel ACTIVE — torrent traffic is routed through the VPN")
		if server != "" {
			fmt.Printf("    Exit server: %s\n", server)
		}
	case alive:
		yellow.Println("  ⚠  Daemon is running but the tunnel is NOT up — downloads go in the clear.")
		dim.Println("     Check `unarr daemon logs` for a [vpn] line. Common cause: no active")
		dim.Println("     VPN on your account (set it up at https://torrentclaw.com/vpn).")
	default:
		dim.Println("  Daemon not running — start it (`unarr start`) to bring the tunnel up.")
	}

	// ── Optional live provisioning check ──
	if check {
		fmt.Println()
		if cfg.Auth.APIKey == "" {
			yellow.Println("  ⚠  No API key — run `unarr init` first.")
		} else {
			apiURL := cfg.Auth.APIURL
			if apiURL == "" {
				apiURL = "https://torrentclaw.com"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := vpn.FetchConfig(ctx, apiURL, cfg.Auth.APIKey, "unarr/"+Version, cfg.Agent.ID, true)
			cancel()
			switch {
			case err == nil:
				green.Println("  ✓ Account provisioned — a VPN config is available.")
			default:
				yellow.Printf("  ⚠  %s\n", err)
			}
		}
	}

	// ── Split-tunnel reminder ──
	fmt.Println()
	dim.Println("  Split-tunnel: only your downloads use the VPN. Your browser and other")
	dim.Println("  apps keep your real IP — that's by design. Use the OpenVPN credentials in")
	dim.Println("  your profile to protect your other devices.")
	fmt.Println()
	return nil
}

func newVPNEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "enable",
		Short:   "Turn the managed VPN on",
		Example: "  unarr vpn enable",
		RunE: func(cmd *cobra.Command, args []string) error {
			return setVPNEnabled(true)
		},
	}
}

func newVPNDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "disable",
		Short:   "Turn the managed VPN off",
		Example: "  unarr vpn disable",
		RunE: func(cmd *cobra.Command, args []string) error {
			return setVPNEnabled(false)
		},
	}
}

func setVPNEnabled(enabled bool) error {
	green := color.New(color.FgGreen)
	yellow := color.New(color.FgYellow)
	dim := color.New(color.FgHiBlack)

	cfg := loadConfig()

	if enabled && cfg.Auth.APIKey == "" {
		return fmt.Errorf("no API key configured — run `unarr init` first (the managed VPN fetches its config from your account)")
	}

	if cfg.Download.VPN.Enabled == enabled {
		fmt.Println()
		dim.Printf("  VPN is already %s — nothing to do.\n", enabledWord(enabled))
		fmt.Println()
		return nil
	}

	cfg.Download.VPN.Enabled = enabled

	configPath := config.FilePath()
	if cfgFile != "" {
		configPath = cfgFile
	}
	if err := config.Save(cfg, configPath); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	appCfg = cfg

	fmt.Println()
	green.Printf("  ✓ Managed VPN %s.\n", enabledWord(enabled))

	if enabled && cfg.Download.VPN.ConfigFile != "" {
		yellow.Println("  ⚠  A config_file is set, so self-hosted mode takes precedence and the")
		yellow.Println("     managed config from your account is ignored. Clear config_file to use it.")
	}

	// The tunnel is brought up once at daemon startup; a plain config reload does
	// NOT (re)create it. Tell the user to restart the daemon if it's running.
	if state := agent.ReadState(); state != nil && isDaemonAlive(state) {
		fmt.Println()
		dim.Println("  The daemon is running. Restart it for this to take effect:")
		dim.Println("    unarr daemon restart")
	}
	fmt.Println()
	return nil
}

func enabledWord(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}
