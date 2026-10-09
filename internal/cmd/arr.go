package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/arr"
	"github.com/spf13/cobra"
)

func newArrCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "arr",
		Short: "Work with your Sonarr / Radarr",
	}
	cmd.AddCommand(newArrSyncCmd())
	return cmd
}

type arrSyncOpts struct {
	SonarrURL, SonarrKey string
	RadarrURL, RadarrKey string
	Backfill, DryRun     bool
	Days                 int
	IndexerMatch         string
}

func newArrSyncCmd() *cobra.Command {
	var o arrSyncOpts
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Mark already-imported files as TrueSpec-verified in Sonarr/Radarr",
		Long: `Sets (or clears) the "Internal" indexer flag on the files Sonarr/Radarr
have already imported, following TorrentClaw's TrueSpec verification.

Why: Sonarr/Radarr score an imported file by its own name and the indexer
flags saved when it was grabbed. A file imported before TrueSpec verified it
(or added by hand) has no flag, so the verified release in a search scores
higher than the file on disk and gets downloaded again. This fixes that
without downloading, renaming or deleting anything: it only rewrites the
indexer flags of the files.

Privacy: only 5-character prefixes of each infohash are sent to TorrentClaw;
the exact match is done locally. Your library never leaves the machine.

Run it once with --backfill before importing the TrueSpec Custom Format, then
periodically (e.g. from cron). Without --backfill only the last --days days
are looked at.`,
		Example: `  unarr arr sync --backfill --dry-run   # preview over the whole history
  unarr arr sync --backfill             # apply once
  unarr arr sync                        # last 30 days (cron)
  unarr arr sync --sonarr-url http://sonarr:8989 --sonarr-key KEY`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runArrSync(cmd.Context(), cmd.OutOrStdout(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.SonarrURL, "sonarr-url", "", "Sonarr URL (skip auto-detection)")
	f.StringVar(&o.SonarrKey, "sonarr-key", "", "Sonarr API key")
	f.StringVar(&o.RadarrURL, "radarr-url", "", "Radarr URL (skip auto-detection)")
	f.StringVar(&o.RadarrKey, "radarr-key", "", "Radarr API key")
	f.BoolVar(&o.Backfill, "backfill", false, "walk the whole history instead of the last --days")
	f.BoolVar(&o.DryRun, "dry-run", false, "show what would change without writing")
	f.IntVar(&o.Days, "days", 30, "history window in days (ignored with --backfill)")
	f.StringVar(&o.IndexerMatch, "indexer-match", "torrentclaw",
		"name (substring) of your TorrentClaw indexer in Sonarr/Radarr")
	return cmd
}

func runArrSync(ctx context.Context, out io.Writer, o arrSyncOpts) error {
	cfg := loadConfig()
	if cfg.Auth.APIKey == "" {
		return fmt.Errorf("unarr is not configured yet — %s", setupHint(cfg.Auth.APIURL))
	}
	instances := selectSyncInstances(o)
	if len(instances) == 0 {
		return fmt.Errorf("no Sonarr/Radarr found — pass --sonarr-url/--sonarr-key and/or --radarr-url/--radarr-key")
	}

	ac := agent.NewClient(apiURLOrDefault(cfg), cfg.Auth.APIKey, "unarr/"+Version)
	opts := arr.SyncOptions{TCMatch: o.IndexerMatch, DryRun: o.DryRun}
	if !o.Backfill {
		opts.Since = time.Now().AddDate(0, 0, -o.Days)
	}

	var failed bool
	for i := range instances {
		inst := &instances[i]
		client := arr.NewClient(inst.URL, inst.APIKey)
		rep, err := arr.SyncTrueSpecFlags(ctx, client, inst.App, trueSpecLookup(ac), opts)
		if err != nil {
			failed = true
			fmt.Fprintf(out, "%s: %v\n", inst.App, err)
			continue
		}
		printSyncReport(out, rep)
	}
	if failed {
		return errQuietExit
	}
	return nil
}

// selectSyncInstances uses the flags when given and auto-detects the rest.
func selectSyncInstances(o arrSyncOpts) []arr.Instance {
	var out []arr.Instance
	have := map[string]bool{}
	add := func(app, url, key string) {
		if url != "" && strings.TrimSpace(key) != "" {
			out = append(out, arr.Instance{App: app, URL: url, APIKey: strings.TrimSpace(key), Source: "manual"})
			have[app] = true
		}
	}
	add("sonarr", o.SonarrURL, o.SonarrKey)
	add("radarr", o.RadarrURL, o.RadarrKey)
	if o.SonarrURL != "" || o.RadarrURL != "" {
		return out
	}
	for _, inst := range arr.Discover() {
		if (inst.App == "sonarr" || inst.App == "radarr") && !have[inst.App] {
			have[inst.App] = true
			out = append(out, inst)
		}
	}
	return out
}

// trueSpecLookup adapts the API client to the lookup the sync expects.
func trueSpecLookup(ac *agent.Client) arr.LookupFunc {
	return func(ctx context.Context, prefixes []string) (map[string]arr.TrueSpecState, error) {
		res, err := ac.TrueSpecLookup(ctx, prefixes)
		if err != nil {
			return nil, err
		}
		out := make(map[string]arr.TrueSpecState, len(res))
		for _, r := range res {
			out[strings.ToLower(r.InfoHash)] = arr.TrueSpecState{Verified: r.Verified, Mismatch: r.Mismatch}
		}
		return out, nil
	}
}

func printSyncReport(out io.Writer, rep *arr.SyncReport) {
	fmt.Fprintf(out, "== %s ==\n", rep.App)
	for _, c := range rep.Changes {
		fmt.Fprintf(out, "  file %d: flags %d → %d  (infohash %s…, verified=%t, grabbed from TorrentClaw=%t)\n",
			c.ID, c.Old, c.Flags, c.Hash[:8], c.Verified, c.FromTC)
	}
	verb := "to fix"
	if rep.Applied {
		verb = "fixed"
	}
	fmt.Fprintf(out, "  %d file(s) %s of %d checked\n", len(rep.Changes), verb, rep.Checked)
}
