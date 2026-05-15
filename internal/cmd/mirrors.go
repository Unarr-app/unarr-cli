package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/config"
)

// newMirrorsCmd wires `unarr mirrors` and its subcommands.
//
// Mirrors are alternate base URLs the agent can fall back to when the
// primary api_url is unreachable. The pool is consulted on every transient
// network failure (DNS, refused, timeout, 5xx) — see internal/agent/
// mirror_pool.go for the rotation rules.
func newMirrorsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mirrors",
		Short: "Manage TorrentClaw mirror failover list",
		Long: `Mirrors are alternate base URLs the agent falls back to when the primary
domain is unreachable. The pool survives DNS blocks, ISP filters, and
short-lived takedowns without restarting the agent.

Examples:
  unarr mirrors list          Print currently configured mirrors
  unarr mirrors update        Refresh from the server's canonical list
  unarr mirrors test          Probe every configured mirror`,
	}

	cmd.AddCommand(newMirrorsListCmd())
	cmd.AddCommand(newMirrorsUpdateCmd())
	cmd.AddCommand(newMirrorsTestCmd())
	return cmd
}

func newMirrorsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Print currently configured mirrors",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfig()
			pool := agent.NewMirrorPool(cfg.Auth.APIURL, cfg.Auth.Mirrors)

			if jsonOut {
				out := map[string]any{
					"primary": cfg.Auth.APIURL,
					"mirrors": pool.Mirrors(),
				}
				return json.NewEncoder(os.Stdout).Encode(out)
			}

			fmt.Printf("Primary: %s\n", color.GreenString(cfg.Auth.APIURL))
			if len(cfg.Auth.Mirrors) == 0 {
				fmt.Println("Fallbacks: (none configured — run `unarr mirrors update`)")
				return nil
			}
			fmt.Println("Fallbacks:")
			for i, m := range cfg.Auth.Mirrors {
				fmt.Printf("  %d. %s\n", i+1, m)
			}
			return nil
		},
	}
}

func newMirrorsUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Refresh the mirror list from the server",
		Long: `Fetch /api/v1/mirrors from the configured primary (with fallback to any
currently-known mirrors) and write the resulting list back to config.toml.

This is how long-running agents survive a takedown of the primary domain:
the user runs ` + "`unarr mirrors update`" + ` once a week (or via cron), and
the agent transparently picks up new mirrors without a CLI release.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfig()

			// Candidate set: primary + any currently-known mirrors. Order matters —
			// we try primary first so the most-trusted endpoint wins.
			candidates := append([]string{cfg.Auth.APIURL}, cfg.Auth.Mirrors...)

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			fmt.Println("Refreshing mirror list...")
			resp, err := agent.FetchMirrorsWithFallback(ctx, candidates, "unarr/"+Version)
			if err != nil {
				return fmt.Errorf("fetch mirrors: %w", err)
			}

			primary, extras := resp.ToConfig()
			if primary == "" {
				return fmt.Errorf("server returned no mirrors")
			}

			// Track what changed so we can give the user a clear diff.
			added, removed := diffMirrors(append([]string{cfg.Auth.APIURL}, cfg.Auth.Mirrors...), append([]string{primary}, extras...))

			cfg.Auth.APIURL = primary
			cfg.Auth.Mirrors = extras
			if err := config.Save(cfg, cfgFile); err != nil {
				return fmt.Errorf("save config: %w", err)
			}

			fmt.Printf("%s revision %d (%d mirror%s)\n",
				color.GreenString("✓"), resp.Revision, len(resp.Mirrors), pluralS(len(resp.Mirrors)))
			fmt.Printf("  Primary:   %s\n", primary)
			if len(extras) > 0 {
				fmt.Printf("  Fallbacks: %s\n", strings.Join(extras, ", "))
			}
			if resp.Tor != nil {
				fmt.Printf("  Tor:       %s\n", resp.Tor.URL)
			}
			for _, c := range resp.Channels {
				fmt.Printf("  Channel:   %s — %s\n", c.Label, c.URL)
			}
			if len(added) > 0 {
				fmt.Printf("  %s %s\n", color.GreenString("added:"), strings.Join(added, ", "))
			}
			if len(removed) > 0 {
				fmt.Printf("  %s %s\n", color.YellowString("removed:"), strings.Join(removed, ", "))
			}
			return nil
		},
	}
}

func newMirrorsTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test",
		Short: "Probe every configured mirror",
		Long: `Performs a small unauthenticated HEAD/GET against /api/health on every
configured mirror and reports latency + reachability.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfig()
			all := append([]string{cfg.Auth.APIURL}, cfg.Auth.Mirrors...)
			if len(all) == 0 {
				return fmt.Errorf("no mirrors configured")
			}

			for _, base := range all {
				if base == "" {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				start := time.Now()
				_, err := agent.FetchMirrors(ctx, []string{base}, "unarr/"+Version)
				cancel()
				elapsed := time.Since(start)
				if err != nil {
					fmt.Printf("  %s %s — %s (%s)\n", color.RedString("✗"), base, err, elapsed.Round(time.Millisecond))
					continue
				}
				fmt.Printf("  %s %s (%s)\n", color.GreenString("✓"), base, elapsed.Round(time.Millisecond))
			}
			return nil
		},
	}
}

// diffMirrors returns the URLs added and removed between two ordered lists.
// Used to print a friendly diff after `unarr mirrors update`.
func diffMirrors(old, fresh []string) (added, removed []string) {
	oldSet := make(map[string]struct{}, len(old))
	for _, m := range old {
		if m != "" {
			oldSet[m] = struct{}{}
		}
	}
	freshSet := make(map[string]struct{}, len(fresh))
	for _, m := range fresh {
		if m == "" {
			continue
		}
		freshSet[m] = struct{}{}
		if _, ok := oldSet[m]; !ok {
			added = append(added, m)
		}
	}
	for _, m := range old {
		if m == "" {
			continue
		}
		if _, ok := freshSet[m]; !ok {
			removed = append(removed, m)
		}
	}
	return added, removed
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
