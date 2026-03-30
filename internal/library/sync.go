package library

import "github.com/torrentclaw/unarr/internal/agent"

// BuildSyncItems converts cached library items to sync request items.
// Shared between unarr scan (cmd/scan.go) and auto-scan (cmd/daemon.go).
func BuildSyncItems(cache *LibraryCache) []agent.LibrarySyncItem {
	items := make([]agent.LibrarySyncItem, 0, len(cache.Items))
	for _, item := range cache.Items {
		if item.ScanError != "" {
			continue
		}
		si := agent.LibrarySyncItem{
			FilePath:    item.FilePath,
			FileName:    item.FileName,
			FileSize:    item.FileSize,
			Title:       item.Title,
			Year:        item.Year,
			ContentType: DeriveContentType(item),
			Season:      item.Season,
			Episode:     item.Episode,
		}

		if item.MediaInfo != nil {
			if item.MediaInfo.Video != nil {
				si.Resolution = ResolveResolution(item.MediaInfo.Video.Height)
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
		}

		items = append(items, si)
	}
	return items
}
