package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	arr.SetUserAgent("unarr/" + Version)
	opts := arr.SyncOptions{TCMatch: o.IndexerMatch, DryRun: o.DryRun}
	if !o.Backfill {
		opts.Since = time.Now().AddDate(0, 0, -o.Days)
	}

	var failed bool
	for i := range instances {
		inst := &instances[i]
		client := arr.NewClient(inst.URL, inst.APIKey).WithTimeout(arrSyncTimeout)
		rep, err := arr.SyncTrueSpecFlags(ctx, client, inst.App, trueSpecLookup(ac), opts)
		if err != nil {
			failed = true
			// stderr, so a cron redirecting stdout to a log still shows the failure
			// and "nothing was changed" is explicit — the sync never writes partially.
			fmt.Fprintf(os.Stderr, "%s: %v - nothing was changed in %s\n", inst.App, describeSyncError(err), inst.App)
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

// lookupRetries is how many times a 429/5xx from TorrentClaw is retried with
// a growing pause before the sync gives up (a backfill of a big library sends
// hundreds of lookups in a row and a single 429 must not void the whole run).
const lookupRetries = 4

// maxRetryPause caps how long a server's Retry-After can make the sync wait.
const maxRetryPause = 120 * time.Second

// arrSyncTimeout is the per-request timeout toward Sonarr/Radarr for the sync.
const arrSyncTimeout = 60 * time.Second

var lookupSleep = time.Sleep // swapped in tests

// trueSpecLookup adapts the API client to the lookup the sync expects.
func trueSpecLookup(ac *agent.Client) arr.LookupFunc {
	return func(ctx context.Context, prefixes []string) (map[string]arr.TrueSpecState, error) {
		var res []agent.TrueSpecLookupResult
		var err error
		for attempt := 1; ; attempt++ {
			res, err = ac.TrueSpecLookup(ctx, prefixes)
			var he *agent.HTTPError
			retryable := errors.As(err, &he) && (he.StatusCode == 429 || he.StatusCode >= 500)
			if err == nil || !retryable || attempt > lookupRetries {
				break
			}
			lookupSleep(retryPause(he, attempt))
		}
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

// retryPause honours the server's Retry-After (capped); without one, it grows
// with the attempt number.
func retryPause(he *agent.HTTPError, attempt int) time.Duration {
	if he.RetryAfter > 0 {
		return min(he.RetryAfter, maxRetryPause)
	}
	return time.Duration(attempt*5) * time.Second
}

// describeSyncError turns the TorrentClaw-side HTTP verdicts into the sentence
// a user can act on; anything else is returned as is.
func describeSyncError(err error) string {
	var he *agent.HTTPError
	if errors.As(err, &he) {
		switch he.StatusCode {
		case 401:
			return "TorrentClaw rejected the API key (401) — run `unarr login` again"
		case 403:
			return "TorrentClaw denied access (403): key without permission, or a network/Cloudflare block"
		case 429:
			return "TorrentClaw rate limit (429) — wait and retry; a FREE key has 120 req/min and 1,000/day shared with Prowlarr"
		}
		if he.StatusCode >= 500 {
			return fmt.Sprintf("TorrentClaw is failing (HTTP %d) — try again in a few minutes", he.StatusCode)
		}
	}
	return err.Error()
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
