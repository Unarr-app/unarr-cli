package library

import (
	"testing"

	"github.com/torrentclaw/unarr/internal/library/mediainfo"
)

func TestBuildSyncItems(t *testing.T) {
	cache := &LibraryCache{
		Items: []LibraryItem{
			{
				FilePath: "/media/movies/Inception.mkv",
				FileName: "Inception.2010.1080p.mkv",
				FileSize: 5000000000,
				Title:    "Inception",
				Year:     "2010",
				MediaInfo: &mediainfo.MediaInfo{
					Video: &mediainfo.VideoInfo{
						Codec:    "hevc",
						Width:    1920,
						Height:   1080,
						BitDepth: 10,
						HDR:      "HDR10",
					},
					Audio: []mediainfo.AudioTrack{
						{Lang: "en", Codec: "ac3", Channels: 6, Default: true},
						{Lang: "es", Codec: "aac", Channels: 2},
					},
					Subtitles: []mediainfo.SubtitleTrack{
						{Lang: "en", Codec: "subrip"},
						{Lang: "es", Codec: "subrip"},
					},
				},
			},
			{
				FilePath: "/media/shows/Breaking.Bad.S01E01.mkv",
				FileName: "Breaking.Bad.S01E01.mkv",
				FileSize: 1000000000,
				Title:    "Breaking Bad",
				Season:   1,
				Episode:  1,
			},
			{
				// Item with scan error — should be skipped
				FilePath:  "/media/bad.mkv",
				FileName:  "bad.mkv",
				ScanError: "ffprobe failed",
			},
		},
	}

	items := BuildSyncItems(cache)

	if len(items) != 2 {
		t.Fatalf("expected 2 items (1 skipped), got %d", len(items))
	}

	// First item: movie with full media info
	movie := items[0]
	if movie.Title != "Inception" {
		t.Errorf("title = %q, want Inception", movie.Title)
	}
	if movie.ContentType != "movie" {
		t.Errorf("contentType = %q, want movie", movie.ContentType)
	}
	if movie.Resolution != "1080p" {
		t.Errorf("resolution = %q, want 1080p", movie.Resolution)
	}
	if movie.VideoCodec != "hevc" {
		t.Errorf("videoCodec = %q, want hevc", movie.VideoCodec)
	}
	if movie.HDR != "HDR10" {
		t.Errorf("hdr = %q, want HDR10", movie.HDR)
	}
	if movie.AudioCodec != "ac3" {
		t.Errorf("audioCodec = %q, want ac3", movie.AudioCodec)
	}
	if movie.AudioChannels != 6 {
		t.Errorf("audioChannels = %d, want 6", movie.AudioChannels)
	}
	if len(movie.AudioLanguages) != 2 {
		t.Errorf("audioLanguages count = %d, want 2", len(movie.AudioLanguages))
	}
	if len(movie.SubtitleLanguages) != 2 {
		t.Errorf("subtitleLanguages count = %d, want 2", len(movie.SubtitleLanguages))
	}

	// Second item: show without media info
	show := items[1]
	if show.ContentType != "show" {
		t.Errorf("contentType = %q, want show", show.ContentType)
	}
	if show.Season != 1 || show.Episode != 1 {
		t.Errorf("season/episode = %d/%d, want 1/1", show.Season, show.Episode)
	}
	if show.Resolution != "" {
		t.Errorf("resolution should be empty, got %q", show.Resolution)
	}
}

func TestBuildSyncItemsEmpty(t *testing.T) {
	cache := &LibraryCache{Items: nil}
	items := BuildSyncItems(cache)
	if len(items) != 0 {
		t.Errorf("expected 0 items, got %d", len(items))
	}
}
