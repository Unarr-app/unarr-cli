package library

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// SyncOptions describes ONE library sync session — a set of batches sharing a
// single syncStartedAt so the server can reap rows not seen by the session.
type SyncOptions struct {
	AgentID string
	// ScanPath is the primary root, kept for pre-scanRoots servers.
	ScanPath string
	// ScanRoots lists every root this session covers (see LibrarySyncRequest).
	ScanRoots []string
	// FullCycle: the session spans every configured root — the server may reap
	// unseen rows regardless of path prefix. NEVER set it for a subtree scan.
	FullCycle bool
	// OnProgress, when non-nil, is called after each batch with (sent, total).
	OnProgress func(sent, total int)
}

// SyncResult aggregates the per-batch server responses of a session.
type SyncResult struct {
	Synced  int
	Matched int
	Removed int
}

// SyncBatches uploads items to the server in batches of 100 as ONE sync
// session: every batch shares the same syncStartedAt and only the final one
// carries isLastBatch, so the server's stale-row cleanup sees the whole cycle
// at once. The single source of the batching protocol — shared by `unarr scan`
// (cmd/scan.go) and the daemon auto-scan (cmd/daemon.go); before this each
// root synced as its own session and the per-agent cleanup could reap rows of
// roots the session never visited.
func SyncBatches(ctx context.Context, ac *agent.Client, items []agent.LibrarySyncItem, opts SyncOptions) (SyncResult, error) {
	const batchSize = 100
	var res SyncResult
	syncStartedAt := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < len(items); i += batchSize {
		end := i + batchSize
		if end > len(items) {
			end = len(items)
		}
		resp, err := ac.SyncLibrary(ctx, agent.LibrarySyncRequest{
			Items:         items[i:end],
			ScanPath:      opts.ScanPath,
			AgentID:       opts.AgentID,
			IsLastBatch:   end >= len(items),
			SyncStartedAt: syncStartedAt,
			ScanRoots:     opts.ScanRoots,
			FullCycle:     opts.FullCycle,
		})
		if err != nil {
			return res, err
		}
		res.Synced += resp.Synced
		res.Matched += resp.Matched
		res.Removed += resp.Removed
		if opts.OnProgress != nil {
			opts.OnProgress(end, len(items))
		}
	}
	return res, nil
}

// relToRoot returns the file's path relative to the scan root (forward-slashed),
// or "" when it doesn't live under root. The server stores this so streaming can
// later reconstruct the absolute path from the agent's *current* root.
func relToRoot(root, full string) string {
	if root == "" {
		return ""
	}
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(rel)
}

// CountAborted returns how many scanned items had an inconclusive probe. Those
// are omitted from the sync payload, so a caller that declares FullCycle must
// check this first — otherwise the server reaps the omitted rows as deleted.
func CountAborted(cache *LibraryCache) int {
	if cache == nil {
		return 0
	}
	n := 0
	for _, item := range cache.Items {
		if item.ScanAborted {
			n++
		}
	}
	return n
}

// PreserveUncoveredItems returns scanned plus every cached item belonging to a
// root this cycle did NOT cover, so saving the cache can't erase work a failed
// or interrupted root had already done.
//
// The auto-scan saves ONE merged cache across all roots. A root whose Scan
// returned an error contributes nothing to that merge, so writing it as-is
// would drop every item under that root — and the next cycle would re-probe it
// from scratch. On a large library that is exactly the "never finishes" trap,
// so an interrupted scan must never cost previously-earned progress.
//
// Only UNCOVERED roots are preserved. A root that scanned cleanly is a complete
// statement of what it holds, so an item missing from it is a deleted file and
// must stay deleted.
func PreserveUncoveredItems(existing *LibraryCache, scanned []LibraryItem, coveredRoots []string) []LibraryItem {
	if existing == nil || len(existing.Items) == 0 {
		return scanned
	}
	covered := func(path string) bool {
		for _, root := range coveredRoots {
			if isUnderRoot(root, path) {
				return true
			}
		}
		return false
	}
	seen := make(map[string]struct{}, len(scanned))
	for _, item := range scanned {
		seen[item.FilePath] = struct{}{}
	}
	out := scanned
	for _, item := range existing.Items {
		if _, dup := seen[item.FilePath]; dup || covered(item.FilePath) {
			continue
		}
		out = append(out, item)
	}
	return out
}

// isUnderRoot reports whether path lives inside root. Uses filepath.Rel rather
// than a string prefix so "/media/tv" doesn't swallow "/media/tv-extras".
func isUnderRoot(root, path string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// BuildSyncItems converts cached library items to sync request items.
// Shared between unarr scan (cmd/scan.go) and auto-scan (cmd/daemon.go).
func BuildSyncItems(cache *LibraryCache) []agent.LibrarySyncItem {
	items := make([]agent.LibrarySyncItem, 0, len(cache.Items))
	for _, item := range cache.Items {
		if item.ScanAborted {
			// The probe never reached a verdict about the FILE (context cancelled,
			// timeout, OOM-kill, mount blip). Syncing these as damaged is what
			// flagged ~1.4k healthy files fleet-wide (2026-07-21) — a single daemon
			// restart mid-scan condemned every file left in the queue. Omitting the
			// row entirely leaves the server's existing verdict (and metadata)
			// untouched; the next clean scan re-probes it.
			continue
		}
		if item.ScanError != "" {
			// A file ffprobe can't read is almost always a truncated/corrupt
			// download (2026-06-15 NFS write-back truncation). Previously these were
			// silently dropped — the file vanished from the library with no trace.
			// Emit a minimal DAMAGED row instead so the web flags it (badge +
			// blocked playback + re-download) rather than hiding it. All fields below
			// are populated before ffprobe runs, so they're valid even on scan error.
			// The scanner re-probes damaged items every scan, so a clean re-download
			// to the same path self-heals the verdict.
			items = append(items, agent.LibrarySyncItem{
				FilePath:        item.FilePath,
				FileName:        item.FileName,
				FileSize:        item.FileSize,
				Title:           item.Title,
				Year:            item.Year,
				ContentType:     DeriveContentType(item),
				Season:          item.Season,
				Episode:         item.Episode,
				Fingerprint:     item.Fingerprint,
				RelPath:         relToRoot(cache.Path, item.FilePath),
				LibraryRootKey:  "library",
				Integrity:       "damaged",
				IntegrityReason: "unreadable",
			})
			continue
		}
		si := agent.LibrarySyncItem{
			FilePath:       item.FilePath,
			FileName:       item.FileName,
			FileSize:       item.FileSize,
			Title:          item.Title,
			Year:           item.Year,
			ContentType:    DeriveContentType(item),
			Season:         item.Season,
			Episode:        item.Episode,
			Fingerprint:    item.Fingerprint,
			RelPath:        relToRoot(cache.Path, item.FilePath),
			LibraryRootKey: "library",
		}

		if item.MediaInfo != nil {
			if item.MediaInfo.Video != nil {
				si.Resolution = ResolveResolution(item.MediaInfo.Video.Width, item.MediaInfo.Video.Height)
				si.VideoCodec = item.MediaInfo.Video.Codec
				si.HDR = item.MediaInfo.Video.HDR
				si.BitDepth = item.MediaInfo.Video.BitDepth
			}
			codec, channels := PrimaryAudioTrack(item.MediaInfo.Audio)
			si.AudioCodec = codec
			si.AudioChannels = channels
			si.AudioLanguages = AudioLanguages(item.MediaInfo.Audio)
			si.SubtitleLanguages = SubtitleLanguages(item.MediaInfo.Subtitles)
			si.AudioTracks = item.MediaInfo.Audio
			si.SubtitleTracks = item.MediaInfo.Subtitles
			si.VideoInfo = item.MediaInfo.Video
			// Only an affirmative damaged verdict is ever sent. An Unverified entry
			// (deep probe timed out even on the serial retry) deliberately falls
			// through: it carries Damaged=false, so it syncs as a normal healthy-
			// looking row and the server keeps whatever it already knew. Do NOT
			// turn "we couldn't check" into "damaged" — that inversion is exactly
			// what flagged ~1.4k good files fleet-wide (2026-07-21).
			if integ := item.MediaInfo.Integrity; integ != nil && integ.Damaged {
				si.Integrity = "damaged"
				si.IntegrityReason = integ.Reason
			}
		}

		items = append(items, si)
	}
	return items
}
