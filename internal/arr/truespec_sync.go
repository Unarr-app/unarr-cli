package arr

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// PrefixLen is how many hex characters of an infohash leave the machine. The
// server answers with every torrent that starts with it (a handful), and the
// exact match is done locally, so TorrentClaw never learns the library.
const PrefixLen = 5

// maxPrefixesPerCall mirrors the server limit of POST /api/v1/truespec/lookup.
const maxPrefixesPerCall = 100

// LookupFunc asks TorrentClaw about hash prefixes (PrefixLen hex chars each)
// and returns the state of every matching infohash, lowercase.
type LookupFunc func(ctx context.Context, prefixes []string) (map[string]TrueSpecState, error)

// SyncOptions tunes one reconciliation.
type SyncOptions struct {
	// Since limits the history window; zero walks the whole history (--backfill).
	Since time.Time
	// TCMatch identifies TorrentClaw in the grab's indexer name.
	TCMatch string
	// DryRun computes the plan without writing it.
	DryRun bool
}

// SyncReport is the outcome for one app.
type SyncReport struct {
	App     string
	Checked int
	Changes []FlagChange
	Applied bool
}

// SyncTrueSpecFlags makes the Internal flag of the files *arr already imported
// agree with TrueSpec, so a Custom Format on that flag scores the file on disk
// the same as the release in the search and *arr stops re-grabbing it.
func SyncTrueSpecFlags(ctx context.Context, c *Client, app string, lookup LookupFunc, opts SyncOptions) (*SyncReport, error) {
	if InternalFlag(app) == 0 {
		return nil, fmt.Errorf("%s has no per-file indexer flags", app)
	}
	imports, grabs, err := c.fetchHistory(opts.Since)
	if err != nil {
		return nil, err
	}
	hashOf := LatestImports(imports, opts.Since)
	report := &SyncReport{App: app}
	if len(hashOf) == 0 {
		return report, nil
	}

	files, err := c.MediaFiles(app, sortedIDs(hashOf))
	if err != nil {
		return nil, err
	}
	if err := requireFlagsExposed(app, files); err != nil {
		return nil, err
	}
	report.Checked = len(files)

	state, err := lookupAll(ctx, lookup, prefixesOf(hashOf))
	if err != nil {
		return nil, err
	}
	report.Changes = PlanFlagChanges(FlagInputs{
		App: app, Files: files, HashOf: hashOf, IndexerOf: GrabIndexers(grabs),
		TrueSpec: state, TCMatch: opts.TCMatch,
	})
	if opts.DryRun || len(report.Changes) == 0 {
		return report, nil
	}
	if err := c.SetIndexerFlags(app, report.Changes); err != nil {
		return nil, err
	}
	report.Applied = true
	return report, nil
}

func (c *Client) fetchHistory(since time.Time) (imports, grabs []HistoryRecord, err error) {
	var stop func(HistoryRecord) bool
	if !since.IsZero() {
		stop = func(oldest HistoryRecord) bool { return oldest.Date.Before(since) }
	}
	if imports, err = c.historyEvents(eventFolderImported, stop); err != nil {
		return nil, nil, err
	}
	// Grabs are looked up by hash, so the whole history is needed even in a window:
	// the grab of a file imported today may be weeks old.
	if grabs, err = c.historyEvents(eventGrabbed, nil); err != nil {
		return nil, nil, err
	}
	return imports, grabs, nil
}

// requireFlagsExposed refuses to continue on an *arr that does not return
// indexerFlags on its files: writing would silently do nothing useful.
func requireFlagsExposed(app string, files []MediaFile) error {
	for _, f := range files {
		if f.IndexerFlags == nil {
			return fmt.Errorf("this version of %s does not expose indexerFlags on files; nothing was changed", app)
		}
	}
	return nil
}

func sortedIDs(hashOf map[int]string) []int {
	ids := make([]int, 0, len(hashOf))
	for id := range hashOf {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// prefixesOf returns the distinct PrefixLen-char prefixes of the hashes.
func prefixesOf(hashOf map[int]string) []string {
	set := map[string]struct{}{}
	for _, h := range hashOf {
		set[h[:PrefixLen]] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func lookupAll(ctx context.Context, lookup LookupFunc, prefixes []string) (map[string]TrueSpecState, error) {
	all := map[string]TrueSpecState{}
	for start := 0; start < len(prefixes); start += maxPrefixesPerCall {
		end := min(start+maxPrefixesPerCall, len(prefixes))
		got, err := lookup(ctx, prefixes[start:end])
		if err != nil {
			return nil, fmt.Errorf("TrueSpec lookup: %w", err)
		}
		for h, s := range got {
			all[h] = s
		}
	}
	return all, nil
}
