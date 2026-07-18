package cmd

import (
	"fmt"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

func newFunnelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "funnel",
		Short: "Expose the daemon over a public HTTPS hostname via CloudFlare Quick Tunnel",
		Long: `Turn the CloudFlare Quick Tunnel on/off and check its status.

When on, the daemon spawns cloudflared as a child process and registers a
` + "`https://<random>.trycloudflare.com`" + ` hostname tunnelled to its local
HLS server. The torrentclaw.com / torrentclaw.to web player picks the tunnel
URL first so cross-network playback works from any browser without Tailscale
or port forwarding.

Trade-offs:
  • Bytes proxy through CloudFlare. We don't relay; CF does. Preserves the
    TorrentClaw legal posture but means CF sees your traffic shape.
  • Quick Tunnels are anonymous — no CF account required.
  • Hostname is random per session and rotates roughly every 6 h.

Requires the cloudflared binary on PATH. Install:
  Linux  : https://pkg.cloudflare.com (apt) or download from
           https://github.com/cloudflare/cloudflared/releases
  macOS  : brew install cloudflared
  Windows: winget install --id Cloudflare.cloudflared`,
		Example: `  unarr funnel status   # is the tunnel up? what's the URL?
  unarr funnel on       # turn it on
  unarr funnel off      # turn it off`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newFunnelStatusCmd(), newFunnelOnCmd(), newFunnelOffCmd())
	return cmd
}

func newFunnelStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Short:   "Show CloudFlare tunnel configuration + live URL",
		Example: "  unarr funnel status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFunnelStatus()
		},
	}
}

func runFunnelStatus() error {
	bold := color.New(color.Bold)
	dim := color.New(color.FgHiBlack)
	green := color.New(color.FgGreen)
	yellow := color.New(color.FgYellow)
	cyan := color.New(color.FgCyan)

	cfg := loadConfig()

	fmt.Println()
	bold.Println("  CloudFlare Quick Tunnel")
	fmt.Println()

	if !cfg.Download.Funnel.Enabled {
		dim.Println("  Mode:    off")
		fmt.Println()
		dim.Println("  Enable with `unarr funnel on` to give the daemon a public HTTPS URL")
		dim.Println("  so cross-network browser playback works without Tailscale.")
		fmt.Println()
		return nil
	}
	cyan.Println("  Mode:    on")

	state := agent.ReadState()
	alive := state != nil && isDaemonAlive(state)
	fmt.Println()
	switch {
	case alive && state.FunnelURL != "":
		green.Println("  ✓ Tunnel ACTIVE")
		fmt.Printf("    URL:  %s\n", state.FunnelURL)
		fmt.Println()
		dim.Println("  This URL rotates roughly every 6 h. The web player picks it up")
		dim.Println("  automatically — no action needed on your side.")
	case alive:
		yellow.Println("  ⚠  Daemon is running but the tunnel hasn't registered yet.")
		dim.Println("     Check `unarr daemon logs` for a [funnel] line. Common cause:")
		dim.Println("     cloudflared isn't installed on PATH.")
	default:
		dim.Println("  Daemon not running — start it (`unarr start`) to bring the tunnel up.")
	}
	fmt.Println()
	return nil
}

func newFunnelOnCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "on",
		Short:   "Turn the CloudFlare tunnel on",
		Example: "  unarr funnel on",
		RunE: func(cmd *cobra.Command, args []string) error {
			return setFunnelEnabled(true)
		},
	}
}

func newFunnelOffCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "off",
		Short:   "Turn the CloudFlare tunnel off",
		Example: "  unarr funnel off",
		RunE: func(cmd *cobra.Command, args []string) error {
			return setFunnelEnabled(false)
		},
	}
}

func setFunnelEnabled(enabled bool) error {
	green := color.New(color.FgGreen)
	dim := color.New(color.FgHiBlack)

	cfg := loadConfig()
	if cfg.Download.Funnel.Enabled == enabled {
		fmt.Println()
		dim.Printf("  Tunnel is already %s — nothing to do.\n", onOffWord(enabled))
		fmt.Println()
		return nil
	}

	cfg.Download.Funnel.Enabled = enabled

	configPath := config.FilePath()
	if cfgFile != "" {
		configPath = cfgFile
	}
	if err := config.Save(cfg, configPath); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	appCfg = cfg

	fmt.Println()
	green.Printf("  ✓ CloudFlare tunnel %s.\n", onOffWord(enabled))

	// Subprocess is launched/torn down by the daemon at startup; a plain config
	// reload does not bring it up. Prompt for a restart when the daemon is alive.
	if state := agent.ReadState(); state != nil && isDaemonAlive(state) {
		fmt.Println()
		dim.Println("  The daemon is running. Restart it for this to take effect:")
		dim.Println("    unarr daemon restart")
	}
	fmt.Println()
	return nil
}

func onOffWord(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}
