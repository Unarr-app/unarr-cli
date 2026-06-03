package library

import (
	"path/filepath"
	"strings"

	"github.com/torrentclaw/unarr/internal/agent"
)

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
