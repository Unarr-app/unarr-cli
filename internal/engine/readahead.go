package engine

// Torrent stream readahead sizing.
//
// anacrolix's Reader (SetResponsive + SetReadahead) already prioritises the
// pieces in a window ahead of the read position and re-prioritises on Seek —
// so the playhead→piece-priority feedback is built in. The problem was the
// window: a static 5 MiB is only ~1.6s of a 25 Mbps 4K stream, so playback
// outran the download and stalled. Sizing the window by bitrate (~30s of video)
// keeps a real buffer ahead of the playhead.
const (
	readaheadSeconds = 30
	minReadahead     = 8 << 20  // 8 MiB
	maxReadahead     = 96 << 20 // 96 MiB — cap so a seek doesn't waste a huge fetch
	defaultReadahead = 24 << 20 // 24 MiB — when bitrate is unknown (still ~5x the old 5 MiB)
)

// dynamicReadahead returns the bytes-ahead window for a torrent reader given the
// stream's bitrate (bits/sec). Unknown/zero bitrate → a generous default.
func dynamicReadahead(bitrateBps int64) int64 {
	if bitrateBps <= 0 {
		return defaultReadahead
	}
	ra := bitrateBps / 8 * readaheadSeconds
	if ra < minReadahead {
		return minReadahead
	}
	if ra > maxReadahead {
		return maxReadahead
	}
	return ra
}
