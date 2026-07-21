package library

import (
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
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

	// 3 items: the movie, the show, and the scan-error file surfaced as DAMAGED
	// (no longer silently dropped — the web flags it for re-download).
	if len(items) != 3 {
		t.Fatalf("expected 3 items (1 damaged), got %d", len(items))
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

	// Third item: scan-error file surfaced as damaged (unreadable), not skipped.
	damaged := items[2]
	if damaged.FilePath != "/media/bad.mkv" {
		t.Errorf("damaged filePath = %q, want /media/bad.mkv", damaged.FilePath)
	}
	if damaged.Integrity != "damaged" {
		t.Errorf("integrity = %q, want damaged", damaged.Integrity)
	}
	if damaged.IntegrityReason != "unreadable" {
		t.Errorf("integrityReason = %q, want unreadable", damaged.IntegrityReason)
	}
}

func TestBuildSyncItemsEmpty(t *testing.T) {
	cache := &LibraryCache{Items: nil}
	items := BuildSyncItems(cache)
	if len(items) != 0 {
		t.Errorf("expected 0 items, got %d", len(items))
	}
}

// An inconclusive probe (cancelled context, timeout, OOM-kill, mount blip) must
// be OMITTED from the sync payload, never reported as damaged. Syncing these as
// damaged/"unreadable" flagged ~1.4k healthy files fleet-wide (2026-07-21) —
// one daemon restart mid-scan condemned every file left in the scan queue.
func TestBuildSyncItemsSkipsAbortedScans(t *testing.T) {
	cache := &LibraryCache{
		Path: "/media",
		Items: []LibraryItem{
			{FilePath: "/media/good.mkv", FileName: "Good.2024.1080p.mkv", Title: "Good", FileSize: 1},
			{FilePath: "/media/aborted.mkv", FileName: "Aborted.2024.1080p.mkv", Title: "Aborted",
				ScanError: "ffprobe aborted (context canceled)", ScanAborted: true},
		},
	}

	items := BuildSyncItems(cache)
	if len(items) != 1 {
		t.Fatalf("expected the aborted item to be dropped, got %d items", len(items))
	}
	if items[0].FilePath != "/media/good.mkv" {
		t.Errorf("wrong item survived: %q", items[0].FilePath)
	}
	for _, it := range items {
		if it.Integrity == "damaged" {
			t.Errorf("aborted scan leaked a damaged verdict for %q", it.FilePath)
		}
	}
}

// CountAborted gates the caller's fullCycle claim: omitted items mean the
// session no longer describes the whole library, and a fullCycle sync would
// have the server's stale-cleanup DELETE those rows.
func TestCountAborted(t *testing.T) {
	if got := CountAborted(nil); got != 0 {
		t.Errorf("CountAborted(nil) = %d, want 0", got)
	}
	cache := &LibraryCache{Items: []LibraryItem{
		{FilePath: "/a.mkv"},
		{FilePath: "/b.mkv", ScanAborted: true},
		{FilePath: "/c.mkv", ScanError: "invalid data found when processing input"},
		{FilePath: "/d.mkv", ScanAborted: true},
	}}
	if got := CountAborted(cache); got != 2 {
		t.Errorf("CountAborted = %d, want 2 (real scan errors must not count)", got)
	}
}

// A root whose scan failed or was interrupted contributes nothing to the merged
// cache. Saving that merge as-is would erase every item under it, and the next
// cycle would re-probe the whole root from scratch — on a large library that is
// the "never finishes" trap. Uncovered roots must be carried over.
func TestPreserveUncoveredItemsKeepsFailedRoots(t *testing.T) {
	existing := &LibraryCache{Items: []LibraryItem{
		{FilePath: "/media/movies/a.mkv", Title: "A"},
		{FilePath: "/media/movies/gone.mkv", Title: "Gone"},
		{FilePath: "/media/tv/b.mkv", Title: "B"},
		{FilePath: "/media/tv-extras/c.mkv", Title: "C"},
	}}
	// Only /media/movies scanned this cycle; "gone.mkv" is genuinely deleted.
	scanned := []LibraryItem{{FilePath: "/media/movies/a.mkv", Title: "A"}}

	out := PreserveUncoveredItems(existing, scanned, []string{"/media/movies"})

	got := map[string]bool{}
	for _, it := range out {
		got[it.FilePath] = true
	}
	if !got["/media/movies/a.mkv"] {
		t.Error("scanned item lost")
	}
	if got["/media/movies/gone.mkv"] {
		t.Error("deleted file under a COVERED root must not be resurrected")
	}
	if !got["/media/tv/b.mkv"] {
		t.Error("item under an uncovered root was erased — next cycle would re-probe it")
	}
	// Prefix-sibling guard: "/media/tv-extras" must not count as covered by
	// "/media/tv" (a plain strings.HasPrefix would wrongly drop it).
	if !got["/media/tv-extras/c.mkv"] {
		t.Error("prefix sibling wrongly treated as covered")
	}
	if len(out) != 3 {
		t.Errorf("expected 3 items, got %d", len(out))
	}
}

func TestPreserveUncoveredItemsNoExistingCache(t *testing.T) {
	scanned := []LibraryItem{{FilePath: "/media/a.mkv"}}
	if out := PreserveUncoveredItems(nil, scanned, []string{"/media"}); len(out) != 1 {
		t.Errorf("nil cache should pass scanned through, got %d", len(out))
	}
}
