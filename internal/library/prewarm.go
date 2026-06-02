package library

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/torrentclaw/unarr/internal/library/mediainfo"
)

// PrewarmOptions controls scan-time sidecar extraction.
type PrewarmOptions struct {
	FFmpegPath     string // resolved ffmpeg binary; empty disables prewarm
	CacheSubtitles bool   // library.cache_subtitles
	Workers        int    // concurrent ffmpeg jobs (each is heavy); default 2
}

// PrewarmSidecars extracts every text subtitle of every scanned item into the
// hidden ".unarr" sidecar dir next to the media file, so the /sub handler serves
// it instantly at play time (instead of re-running ffmpeg, which on a 50GB+
// remux exceeds the on-demand HTTP timeout). Without the per-request 60s ceiling
// here, even huge files complete (generous per-file timeout).
//
// Best-effort and idempotent: an already-fresh sidecar is skipped, errors are
// logged and the item moves on, and ctx cancellation (Ctrl-C / daemon shutdown)
// stops cleanly. Safe to call after every scan — only missing/stale caches do work.
func PrewarmSidecars(ctx context.Context, cache *LibraryCache, opts PrewarmOptions) {
	if cache == nil || opts.FFmpegPath == "" || !opts.CacheSubtitles {
		return
	}
	workers := opts.Workers
	if workers < 1 {
		workers = 2
	}

	type job struct {
		path  string
		index int
	}
	jobs := make(chan job)
	var wg sync.WaitGroup
	var mu sync.Mutex
	cached, failed := 0, 0

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				if _, ok := mediainfo.ReadCachedSubtitle(j.path, j.index); ok {
					continue // already fresh
				}
				// Generous per-file deadline: a full text track on a multi-GB
				// remux can take minutes to demux. Bounded so one corrupt file
				// can't wedge a worker forever.
				jctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
				vtt, err := mediainfo.ExtractSubtitleVTT(jctx, opts.FFmpegPath, j.path, j.index)
				cancel()
				if err != nil {
					mu.Lock()
					failed++
					mu.Unlock()
					continue
				}
				if werr := mediainfo.WriteCachedSubtitle(j.path, j.index, vtt); werr != nil {
					log.Printf("[prewarm] sidecar write skipped (i=%d path=%q): %v", j.index, j.path, werr)
					mu.Lock()
					failed++
					mu.Unlock()
					continue
				}
				mu.Lock()
				cached++
				mu.Unlock()
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, item := range cache.Items {
			if item.MediaInfo == nil || item.FilePath == "" {
				continue
			}
			for idx, sub := range item.MediaInfo.Subtitles {
				if !mediainfo.IsTextSubtitleCodec(sub.Codec) {
					continue // bitmap → burned in, not extractable to WebVTT
				}
				select {
				case jobs <- job{path: item.FilePath, index: idx}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	wg.Wait()
	if cached > 0 || failed > 0 {
		log.Printf("[prewarm] subtitles: %d cached, %d failed", cached, failed)
	}
}
