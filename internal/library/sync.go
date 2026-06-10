package library

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/torrentclaw/unarr/internal/agent"
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

// BuildSyncItems converts cached library items to sync request items.
// Shared between unarr scan (cmd/scan.go) and auto-scan (cmd/daemon.go).
func BuildSyncItems(cache *LibraryCache) []agent.LibrarySyncItem {
	items := make([]agent.LibrarySyncItem, 0, len(cache.Items))
	for _, item := range cache.Items {
		if item.ScanError != "" {
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
			if integ := item.MediaInfo.Integrity; integ != nil && integ.Damaged {
				si.Integrity = "damaged"
				si.IntegrityReason = integ.Reason
			}
		}

		items = append(items, si)
	}
	return items
}
