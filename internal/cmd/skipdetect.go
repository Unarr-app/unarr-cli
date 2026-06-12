package cmd

import (
	"context"
	"log"

	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/config"
	"github.com/torrentclaw/unarr/internal/library"
	"github.com/torrentclaw/unarr/internal/library/mediainfo"
)

// detectAndSubmitSkipSegments runs intro/credits detection over a scanned
// cache and uploads the results. Called AFTER the library sync (the server
// resolves file paths against the just-synced library_item rows). Best-effort:
// every failure logs and returns — a scan must never fail because of this.
func detectAndSubmitSkipSegments(ctx context.Context, cfg config.Config, ac *agent.Client, cache *library.LibraryCache) {
	if !cfg.Library.SkipDetect || cache == nil || ctx.Err() != nil {
		return
	}
	ffmpegPath, err := mediainfo.ResolveFFmpeg(cfg.Library.FFmpegPath)
	if err != nil {
		log.Printf("[skipdetect] skipped: ffmpeg unavailable: %v", err)
		return
	}
	fpcalcPath, err := mediainfo.ResolveFpcalc()
	if err != nil {
		// Movies-only still works (black frames need just ffmpeg).
		log.Printf("[skipdetect] fpcalc unavailable (episode detection off): %v", err)
		fpcalcPath = ""
	}

	// Two phases so fast results don't wait on slow ones: episode fingerprinting
	// is seconds per season (and often pure cache), while movie black-frame
	// scans grind through 4K tails over the NAS — episodes submit first.
	episodes := library.DetectSkipSegments(ctx, cache, library.SkipDetectOptions{
		FFmpegPath: ffmpegPath,
		FpcalcPath: fpcalcPath,
		Workers:    2,
	})
	submitSkipSegments(ctx, cfg, ac, episodes)

	movies := library.DetectSkipSegments(ctx, cache, library.SkipDetectOptions{
		FFmpegPath: ffmpegPath,
		Workers:    2,
		Movies:     true,
	})
	submitSkipSegments(ctx, cfg, ac, movies)
}

func submitSkipSegments(ctx context.Context, cfg config.Config, ac *agent.Client, detections []library.SkipDetection) {
	if len(detections) == 0 || ac == nil || ctx.Err() != nil {
		return
	}

	items := make([]agent.SkipSegmentItem, 0, len(detections))
	for _, d := range detections {
		segs := make([]agent.SkipSegmentRange, 0, len(d.Segments))
		for _, s := range d.Segments {
			segs = append(segs, agent.SkipSegmentRange{Category: s.Category, StartSec: s.StartSec, EndSec: s.EndSec})
		}
		items = append(items, agent.SkipSegmentItem{
			FilePath:    d.Item.FilePath,
			Title:       d.Item.Title,
			Season:      d.Item.Season,
			Episode:     d.Item.Episode,
			DurationSec: d.DurationSec,
			Segments:    segs,
		})
	}

	const batchSize = 200
	stored, unmatched := 0, 0
	for start := 0; start < len(items); start += batchSize {
		end := start + batchSize
		if end > len(items) {
			end = len(items)
		}
		res, err := ac.SubmitSkipSegments(ctx, agent.SkipSegmentsRequest{
			AgentID: cfg.Agent.ID,
			Items:   items[start:end],
		})
		if err != nil {
			log.Printf("[skipdetect] submit failed: %v", err)
			return
		}
		stored += res.Stored
		unmatched += res.Unmatched
	}
	log.Printf("[skipdetect] submitted %d file(s): %d segment(s) stored, %d unmatched", len(items), stored, unmatched)
}
