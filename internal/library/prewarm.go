package library

import (
	"context"
	"errors"
	"log"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

// Thumbnail sampling — kept in lockstep with the web's src/lib/stream/thumbnails.ts
// (THUMB_FRACTIONS / THUMB_FALLBACK_SECS / THUMB_WIDTH) so the frames the scan
// pre-extracts are the exact ones the "file characteristics" panel requests.
var (
	thumbFractions   = []float64{0.1, 0.3, 0.5, 0.7, 0.9}
	thumbFallbackSec = []float64{30, 120, 300, 600, 1200}
)

const thumbWidth = 320

// PrewarmOptions controls scan-time sidecar extraction.
type PrewarmOptions struct {
	FFmpegPath      string // resolved ffmpeg binary; empty disables prewarm
	CacheSubtitles  bool   // library.cache_subtitles
	CacheThumbnails bool   // library.cache_thumbnails
	Workers         int    // concurrent ffmpeg jobs (each is heavy); default 2

	// Trickplay (library.trickplay): generate ONE montage sprite per file sampled
	// every TrickplayIntervalSec at TrickplayWidth. Replaces live scrubber
	// extraction during playback (no contention with the active stream).
	Trickplay            bool
	TrickplayIntervalSec float64
	TrickplayWidth       int

	// MaxLoadRatio gates the heavy trickplay decode on system load: a job only
	// starts while the 1-min load average is ≤ MaxLoadRatio×NumCPU, so sprite
	// generation never saturates the machine or the NAS. ≤0 → default 0.7. Has no
	// effect on platforms without a load reading (proceeds unthrottled).
	MaxLoadRatio float64
}

// prewarmJob is one extraction unit: all text subtitles of a file in one ffmpeg
// pass (subtitle job), a single thumbnail frame (thumb=true), or the trickplay
// montage sprite for a file (trick=true).
type prewarmJob struct {
	path     string
	thumb    bool
	trick    bool    // trickplay sprite job
	subIdx   []int   // subtitle stream indices to extract in ONE pass (subtitle job)
	posSec   float64 // frame position in seconds (thumbnail job)
	width    int     // frame/tile width (thumbnail + trickplay jobs)
	duration float64 // runtime seconds (trickplay job)
}

// PrewarmSidecars extracts text subtitles (→ WebVTT) and the panel's sample
// thumbnail frames (→ JPEG) for every scanned item into the hidden ".unarr"
// sidecar dir next to the media file, so the /sub and /thumbnail handlers serve
// them instantly. Subtitle extraction without the per-request HTTP timeout is
// what makes huge remuxes work.
//
// Best-effort and idempotent: fresh sidecars are skipped, errors are logged and
// the item moves on, and ctx cancellation (Ctrl-C / daemon shutdown) stops
// cleanly. Safe to call after every scan — only missing/stale caches do work.
func PrewarmSidecars(ctx context.Context, cache *LibraryCache, opts PrewarmOptions) {
	if cache == nil || opts.FFmpegPath == "" || (!opts.CacheSubtitles && !opts.CacheThumbnails && !opts.Trickplay) {
		return
	}
	workers := opts.Workers
	if workers < 1 {
		workers = 2
	}
	maxLoadRatio := opts.MaxLoadRatio
	if maxLoadRatio <= 0 {
		maxLoadRatio = 0.7
	}
	// Trickplay is the heaviest job (full 4K decode). Cap it to ONE concurrent
	// decode across this agent's workers — the thumbnail/subtitle jobs (light /
	// I/O-bound) keep their `workers` parallelism. Cross-agent dup work is stopped
	// by the per-file lock inside GenerateTrickplay.
	trickSem := make(chan struct{}, 1)

	jobs := make(chan prewarmJob)
	var wg sync.WaitGroup
	var mu sync.Mutex
	subCached, thumbCached, trickCached, failed := 0, 0, 0, 0
	var sampleErr string // first extraction error, surfaced in the summary so a
	// systemic ffmpeg failure (vs one corrupt file) is diagnosable from "N failed".

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				if j.thumb {
					if _, ok := mediainfo.ReadCachedThumbnail(j.path, j.posSec, j.width); ok {
						continue
					}
					// A single keyframe decode is fast; 60s bounds a corrupt file.
					jctx, cancel := context.WithTimeout(ctx, 60*time.Second)
					img, err := mediainfo.ExtractThumbnailJPEG(jctx, opts.FFmpegPath, j.path, j.posSec, j.width)
					cancel()
					if err != nil { // seek past EOF / corrupt → skip
						mu.Lock()
						failed++
						if sampleErr == "" {
							sampleErr = err.Error()
						}
						mu.Unlock()
						continue
					}
					if werr := mediainfo.WriteCachedThumbnail(j.path, j.posSec, j.width, img); werr != nil {
						log.Printf("[prewarm] thumbnail write skipped (pos=%.0f path=%q): %v", j.posSec, j.path, werr)
						mu.Lock()
						failed++
						mu.Unlock()
						continue
					}
					mu.Lock()
					thumbCached++
					mu.Unlock()
					continue
				}

				if j.trick {
					if _, ok := mediainfo.ReadCachedTrickplay(j.path, j.width); ok {
						continue
					}
					// Serialize the heavy decode (1 at a time) and wait for the box to
					// be idle enough before starting — sprite generation must never
					// saturate the CPU or the NAS.
					select {
					case trickSem <- struct{}{}:
					case <-ctx.Done():
						return
					}
					waitForLowLoad(ctx, maxLoadRatio)
					if ctx.Err() != nil {
						<-trickSem
						return
					}
					// Full-decode pass (samples 1 frame per interval over the whole
					// file) — generous deadline like subtitles; idempotent + cached.
					// INVARIANT: this deadline MUST stay below mediainfo.trickplayLockTTL,
					// or another agent could reclaim a still-running job's lock and double
					// the decode. If you raise this, raise trickplayLockTTL too.
					jctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
					_, err := mediainfo.GenerateTrickplay(jctx, opts.FFmpegPath, j.path, opts.TrickplayIntervalSec, j.width, j.duration)
					cancel()
					<-trickSem
					mu.Lock()
					switch {
					case err == nil:
						trickCached++
					case errors.Is(err, mediainfo.ErrTrickplayInProgress):
						// another worker/agent owns this file — skip, not a failure.
					default:
						failed++
						if sampleErr == "" {
							sampleErr = err.Error()
						}
					}
					mu.Unlock()
					continue
				}

				// Extract only the indices not already fresh, and do them in ONE
				// ffmpeg pass — a multi-GB remux is demuxed once for all its text
				// tracks instead of once per track.
				todo := make([]int, 0, len(j.subIdx))
				for _, idx := range j.subIdx {
					if _, ok := mediainfo.ReadCachedSubtitle(j.path, idx); !ok {
						todo = append(todo, idx)
					}
				}
				if len(todo) == 0 {
					continue
				}
				// Generous per-file deadline. Subtitle packets are interleaved across
				// the whole container, so extraction is I/O-bound: it must read the
				// entire file once (all text tracks share that single pass). A 60GB
				// remux over ~75 MB/s NFS is ~14 min, so 45 min covers files up to
				// ~200GB; bounded so one corrupt/stalled file can't wedge a worker.
				// This is background + idempotent — it only runs until the cache fills.
				jctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
				res, err := mediainfo.ExtractSubtitlesVTTMulti(jctx, opts.FFmpegPath, j.path, todo)
				cancel()
				if err != nil {
					mu.Lock()
					failed += len(todo)
					if sampleErr == "" {
						sampleErr = err.Error()
					}
					mu.Unlock()
					continue
				}
				for idx, vtt := range res {
					if werr := mediainfo.WriteCachedSubtitle(j.path, idx, vtt); werr != nil {
						log.Printf("[prewarm] sidecar write skipped (i=%d path=%q): %v", idx, j.path, werr)
						mu.Lock()
						failed++
						mu.Unlock()
						continue
					}
					mu.Lock()
					subCached++
					mu.Unlock()
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, item := range cache.Items {
			if item.MediaInfo == nil || item.FilePath == "" {
				continue
			}
			if opts.CacheSubtitles {
				var subIdx []int
				for idx, sub := range item.MediaInfo.Subtitles {
					if mediainfo.IsTextSubtitleCodec(sub.Codec) {
						subIdx = append(subIdx, idx) // bitmap → burned in, skipped
					}
				}
				if len(subIdx) > 0 {
					select {
					case jobs <- prewarmJob{path: item.FilePath, subIdx: subIdx}:
					case <-ctx.Done():
						return
					}
				}
			}
			if opts.CacheThumbnails {
				for _, pos := range thumbPositions(item) {
					select {
					case jobs <- prewarmJob{path: item.FilePath, thumb: true, posSec: pos, width: thumbWidth}:
					case <-ctx.Done():
						return
					}
				}
			}
			if opts.Trickplay && opts.TrickplayIntervalSec > 0 {
				dur := 0.0
				if item.MediaInfo.Video != nil {
					dur = item.MediaInfo.Video.Duration
				}
				if dur > 0 {
					w := opts.TrickplayWidth
					if w <= 0 {
						w = 240
					}
					select {
					case jobs <- prewarmJob{path: item.FilePath, trick: true, width: w, duration: dur}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	wg.Wait()
	if subCached > 0 || thumbCached > 0 || trickCached > 0 || failed > 0 {
		if failed > 0 && sampleErr != "" {
			log.Printf("[prewarm] %d subtitles, %d thumbnails, %d trickplay cached, %d failed (e.g. %s)", subCached, thumbCached, trickCached, failed, sampleErr)
		} else {
			log.Printf("[prewarm] %d subtitles, %d thumbnails, %d trickplay cached, %d failed", subCached, thumbCached, trickCached, failed)
		}
	}
}

// prewarmLoadWaitCap bounds how long the load gate DEFERS a trickplay job. It's a
// throttle, not an off-switch: on a host whose baseline load is permanently above
// the threshold (a shared prod box, or any 1–2 core machine), an unbounded wait
// would mean sprites NEVER generate. After the cap we proceed anyway — the other
// safeguards (single-flight lock, trickSem=1, nice 19 + idle I/O, Pdeathsig) keep
// one throttled decode from saturating the box.
const prewarmLoadWaitCap = 15 * time.Minute

// waitForLowLoad defers until the 1-minute system load is at or below
// max(maxRatio×NumCPU, 1.5), or ctx is cancelled, or prewarmLoadWaitCap elapses —
// so the heavy trickplay decode prefers an idle machine but never stalls forever.
// The 1.5 floor reserves ~one core so the gate can still open on 1–2 core hosts
// (without it, threshold 0.7–1.4 is below almost any active machine's load and the
// feature would be permanently off). No load reading (non-Linux) → returns at once.
func waitForLowLoad(ctx context.Context, maxRatio float64) {
	threshold := maxRatio * float64(runtime.NumCPU())
	if threshold < 1.5 {
		threshold = 1.5
	}
	deadline := time.After(prewarmLoadWaitCap)
	logged := false
	for {
		load, ok := mediainfo.LoadAverage1()
		if !ok || load <= threshold {
			return
		}
		if !logged {
			log.Printf("[prewarm] system load %.1f > %.1f — deferring trickplay (≤ %s)", load, threshold, prewarmLoadWaitCap)
			logged = true
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			log.Printf("[prewarm] load still high after %s — proceeding with throttled trickplay (nice + idle I/O + single-flight still apply)", prewarmLoadWaitCap)
			return
		case <-time.After(15 * time.Second):
		}
	}
}

// thumbPositions returns the sample frame offsets (whole seconds) for an item,
// matching the web panel: fractions of a known runtime, else fixed fallbacks.
func thumbPositions(item LibraryItem) []float64 {
	var dur float64
	if item.MediaInfo != nil && item.MediaInfo.Video != nil {
		dur = item.MediaInfo.Video.Duration
	}
	src := thumbFallbackSec
	if dur > 0 {
		src = make([]float64, len(thumbFractions))
		for i, f := range thumbFractions {
			src[i] = math.Round(dur * f)
		}
	}
	// Dedup (short clips can round multiple fractions to the same second).
	seen := make(map[float64]struct{}, len(src))
	out := make([]float64, 0, len(src))
	for _, p := range src {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
