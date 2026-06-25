package library

import (
	"context"
	"log"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

// Skip-segment detection: find intro (OP) and credits (ED) ranges by comparing
// chromaprint audio fingerprints between episodes of the same season (episodes
// share identical intro/credits audio), plus black-frame credits detection for
// movies (no sibling to compare). Results are cached as ".unarr" sidecars and
// submitted to the web, which shares them across all users by content identity.

const (
	skipMinIntroSec    = 15
	skipMaxIntroSec    = 120
	skipMinCreditsSec  = 15
	skipMaxCreditsSec  = 450
	skipCreditsWindow  = 450 // episodes: fingerprint the last N seconds
	skipIntroWindowCap = 600 // episodes: fingerprint at most the first N seconds
	skipMinRuntimeSec  = 300 // ignore shorts/extras

	movieCreditsWindow = 900 // movies: black-frame scan over the last N seconds
	movieMinCreditsSec = 60
	movieMinRuntimeSec = 3600
)

// SkipDetectOptions configures DetectSkipSegments.
type SkipDetectOptions struct {
	FFmpegPath string
	FpcalcPath string // empty disables episode (chromaprint) detection
	Workers    int    // concurrent ffmpeg+fpcalc jobs; default 2
	Movies     bool   // also detect movie end credits via black frames
}

// SkipDetection is the outcome for one media file (only files with ≥1 segment
// are returned).
type SkipDetection struct {
	Item        LibraryItem
	DurationSec float64
	Segments    []mediainfo.SkipSegmentRange
}

// DetectSkipSegments analyzes the scanned library and returns every file with
// detected skippable segments. Idempotent and best-effort: fresh sidecar
// results are reused without re-analysis, errors skip the file, ctx cancels
// cleanly.
func DetectSkipSegments(ctx context.Context, cache *LibraryCache, opts SkipDetectOptions) []SkipDetection {
	if cache == nil || opts.FFmpegPath == "" {
		return nil
	}
	workers := opts.Workers
	if workers < 1 {
		workers = 2
	}

	var out []SkipDetection
	var outMu sync.Mutex
	add := func(item LibraryItem, dur float64, segs []mediainfo.SkipSegmentRange) {
		if len(segs) == 0 {
			return
		}
		outMu.Lock()
		out = append(out, SkipDetection{Item: item, DurationSec: dur, Segments: segs})
		outMu.Unlock()
	}

	start := time.Now()
	analyzed, cached := 0, 0

	if opts.FpcalcPath != "" {
		a, c := detectEpisodeGroups(ctx, cache, opts, workers, add)
		analyzed += a
		cached += c
	}
	if opts.Movies {
		a, c := detectMovieCredits(ctx, cache, opts, workers, add)
		analyzed += a
		cached += c
	}

	log.Printf("[skipdetect] %d file(s) analyzed (%d from cache), %d with segments, in %s",
		analyzed, cached, len(out), time.Since(start).Round(time.Second))
	return out
}

// seasonEpisodeMarker locates the SxxEyy token in a parsed title so the group
// key uses only the SHOW part. Parsed titles keep the episode name + release
// tags ("Show S01E09 Embrace and Whisper BILI WEB DL…"), which differ per
// file — grouping on the raw title would leave every episode alone.
var seasonEpisodeMarker = regexp.MustCompile(`(?i)\bS\d{1,2}\s*E\d{1,4}\b`)

// seasonGroupKey groups episodes that can share intro/credits audio: same
// directory + same show-title prefix + same season. The directory bound keeps
// flat mixed folders from exploding into one giant group; cross-show pairs
// inside a dir fail closed anyway (unrelated audio never matches).
func seasonGroupKey(item LibraryItem) string {
	title := strings.ToLower(strings.TrimSpace(item.Title))
	if loc := seasonEpisodeMarker.FindStringIndex(title); loc != nil {
		title = strings.TrimSpace(title[:loc[0]])
	}
	return filepath.Dir(item.FilePath) + "|" + title + "|s" + strconv.Itoa(item.Season)
}

func itemDuration(item LibraryItem) float64 {
	if item.MediaInfo != nil && item.MediaInfo.Video != nil {
		return item.MediaInfo.Video.Duration
	}
	return 0
}

// detectEpisodeGroups runs chromaprint comparison inside (title, season)
// groups. Returns (analyzed, fromCache) counters.
func detectEpisodeGroups(ctx context.Context, cache *LibraryCache, opts SkipDetectOptions, workers int, add func(LibraryItem, float64, []mediainfo.SkipSegmentRange)) (int, int) {
	groups := make(map[string][]LibraryItem)
	for _, item := range cache.Items {
		if item.Season <= 0 || item.Episode <= 0 || item.FilePath == "" {
			continue
		}
		if itemDuration(item) < skipMinRuntimeSec {
			continue
		}
		groups[seasonGroupKey(item)] = append(groups[seasonGroupKey(item)], item)
	}

	analyzed, fromCache := 0, 0
	for _, items := range groups {
		if ctx.Err() != nil {
			break
		}
		// Distinct episode numbers — two releases of the same episode carry
		// identical full audio (a comparison would match the whole window).
		eps := make(map[int]struct{})
		for _, it := range items {
			eps[it.Episode] = struct{}{}
		}
		if len(eps) < 2 {
			continue
		}

		// Cached results short-circuit the whole group when complete.
		needCompute := false
		cachedSegs := make(map[string]*mediainfo.SkipSegmentsSidecar, len(items))
		for _, it := range items {
			if sc, ok := mediainfo.ReadCachedSkipSegments(it.FilePath); ok {
				cachedSegs[it.FilePath] = sc
			} else {
				needCompute = true
			}
		}
		if !needCompute {
			for _, it := range items {
				sc := cachedSegs[it.FilePath]
				analyzed++
				fromCache++
				add(it, sc.DurationSec, sc.Segments)
			}
			continue
		}

		// Fingerprint every episode in the group (intro + credits windows).
		fps := fingerprintGroup(ctx, items, opts, workers)

		sort.Slice(items, func(i, j int) bool { return items[i].Episode < items[j].Episode })
		for _, it := range items {
			if ctx.Err() != nil {
				break
			}
			analyzed++
			if sc, ok := cachedSegs[it.FilePath]; ok {
				fromCache++
				add(it, sc.DurationSec, sc.Segments)
				continue
			}
			fp := fps[it.FilePath]
			if fp == nil {
				continue
			}
			segs := detectForEpisode(it, fp, items, fps)
			if err := mediainfo.WriteCachedSkipSegments(it.FilePath, fp.duration, segs); err != nil {
				log.Printf("[skipdetect] sidecar write skipped (%q): %v", it.FilePath, err)
			}
			add(it, fp.duration, segs)
		}
	}
	return analyzed, fromCache
}

// episodeFingerprints holds the two fingerprinted windows of one file.
type episodeFingerprints struct {
	duration     float64
	intro        []uint32
	credits      []uint32
	creditsStart float64 // absolute offset of the credits window
}

func fingerprintGroup(ctx context.Context, items []LibraryItem, opts SkipDetectOptions, workers int) map[string]*episodeFingerprints {
	fps := make(map[string]*episodeFingerprints, len(items))
	var mu sync.Mutex
	jobs := make(chan LibraryItem)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				if ctx.Err() != nil {
					return
				}
				dur := itemDuration(it)
				introWin := math.Min(0.25*dur, skipIntroWindowCap)
				credStart := math.Max(0, dur-skipCreditsWindow)
				jctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				intro, err1 := mediainfo.FingerprintAudioWindow(jctx, opts.FFmpegPath, opts.FpcalcPath, it.FilePath, 0, introWin)
				credits, err2 := mediainfo.FingerprintAudioWindow(jctx, opts.FFmpegPath, opts.FpcalcPath, it.FilePath, credStart, skipCreditsWindow)
				cancel()
				if err1 != nil || err2 != nil {
					if err1 != nil {
						log.Printf("[skipdetect] fingerprint failed (%q): %v", it.FilePath, err1)
					} else {
						log.Printf("[skipdetect] fingerprint failed (%q): %v", it.FilePath, err2)
					}
					continue
				}
				mu.Lock()
				fps[it.FilePath] = &episodeFingerprints{duration: dur, intro: intro, credits: credits, creditsStart: credStart}
				mu.Unlock()
			}
		}()
	}
	for _, it := range items {
		// Skip already-cached files only if every OTHER episode can still find
		// partners — fingerprinting cached files too keeps them available as
		// comparison partners for the new ones, so always fingerprint.
		select {
		case jobs <- it:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	return fps
}

// detectForEpisode compares one episode against partners (nearest different
// episode numbers first, up to 3) and returns its detected segments.
func detectForEpisode(it LibraryItem, fp *episodeFingerprints, items []LibraryItem, fps map[string]*episodeFingerprints) []mediainfo.SkipSegmentRange {
	type partner struct {
		fp   *episodeFingerprints
		dist int
	}
	var partners []partner
	for _, other := range items {
		if other.FilePath == it.FilePath || other.Episode == it.Episode {
			continue
		}
		ofp := fps[other.FilePath]
		if ofp == nil {
			continue
		}
		d := other.Episode - it.Episode
		if d < 0 {
			d = -d
		}
		partners = append(partners, partner{fp: ofp, dist: d})
	}
	sort.Slice(partners, func(i, j int) bool { return partners[i].dist < partners[j].dist })
	if len(partners) > 3 {
		partners = partners[:3]
	}

	segs := make([]mediainfo.SkipSegmentRange, 0, 2)

	for _, p := range partners {
		r := mediainfo.FindSharedRegion(fp.intro, p.fp.intro, skipMinIntroSec, skipMaxIntroSec)
		if r == nil {
			continue
		}
		start, end := r.AStart, r.AEnd
		if start <= 5 { // OP at the head — snap to the very start
			start = 0
		}
		segs = append(segs, mediainfo.SkipSegmentRange{Category: "intro", StartSec: round1(start), EndSec: round1(end)})
		break
	}

	for _, p := range partners {
		// A near-full-window match means the two files share ALL audio (same
		// episode content) — not a credits segment.
		r := mediainfo.FindSharedRegion(fp.credits, p.fp.credits, skipMinCreditsSec, skipMaxCreditsSec)
		if r == nil || r.Duration >= 0.97*skipCreditsWindow {
			continue
		}
		segs = append(segs, mediainfo.SkipSegmentRange{
			Category: "credits",
			StartSec: round1(fp.creditsStart + r.AStart),
			EndSec:   round1(fp.creditsStart + r.AEnd),
		})
		break
	}
	return segs
}

// detectMovieCredits finds end-credits in movies via sustained black-frame
// runs (classic credits roll on black). Single-file, no fingerprinting.
func detectMovieCredits(ctx context.Context, cache *LibraryCache, opts SkipDetectOptions, workers int, add func(LibraryItem, float64, []mediainfo.SkipSegmentRange)) (int, int) {
	var movies []LibraryItem
	for _, item := range cache.Items {
		if item.Season > 0 || item.Episode > 0 || item.FilePath == "" {
			continue
		}
		if itemDuration(item) < movieMinRuntimeSec {
			continue
		}
		movies = append(movies, item)
	}

	analyzed, fromCache := 0, 0
	var mu sync.Mutex
	jobs := make(chan LibraryItem)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				if ctx.Err() != nil {
					return
				}
				dur := itemDuration(it)
				if sc, ok := mediainfo.ReadCachedSkipSegments(it.FilePath); ok {
					mu.Lock()
					analyzed++
					fromCache++
					mu.Unlock()
					add(it, sc.DurationSec, sc.Segments)
					continue
				}
				winStart := math.Max(0, dur-movieCreditsWindow)
				jctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
				times, err := mediainfo.DetectBlackFrameRuns(jctx, opts.FFmpegPath, it.FilePath, winStart, movieCreditsWindow, 85)
				cancel()
				if err != nil {
					log.Printf("[skipdetect] blackframe failed (%q): %v", it.FilePath, err)
					continue
				}
				segs := creditsFromBlackRuns(times, dur)
				if werr := mediainfo.WriteCachedSkipSegments(it.FilePath, dur, segs); werr != nil {
					log.Printf("[skipdetect] sidecar write skipped (%q): %v", it.FilePath, werr)
				}
				mu.Lock()
				analyzed++
				mu.Unlock()
				add(it, dur, segs)
			}
		}()
	}
	for _, it := range movies {
		select {
		case jobs <- it:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	return analyzed, fromCache
}

// creditsFromBlackRuns picks the credits start from black-frame timestamps:
// the longest run of black frames (gaps ≤30s between hits) that reaches the
// end of the file (within 90s — post-credits scenes break the run and are
// kept watchable). Requires ≥60s of credits to avoid fade-to-black scenes.
func creditsFromBlackRuns(times []float64, durationSec float64) []mediainfo.SkipSegmentRange {
	if len(times) == 0 {
		return nil
	}
	const maxGap = 30.0
	bestStart, bestEnd := -1.0, -1.0
	runStart := times[0]
	prev := times[0]
	flush := func(end float64) {
		if end-runStart > bestEnd-bestStart {
			bestStart, bestEnd = runStart, end
		}
	}
	for _, t := range times[1:] {
		if t-prev > maxGap {
			flush(prev)
			runStart = t
		}
		prev = t
	}
	flush(prev)

	if bestStart < 0 || bestEnd-bestStart < movieMinCreditsSec {
		return nil
	}
	if durationSec-bestEnd > 90 { // run doesn't reach the end → mid-film scene
		return nil
	}
	return []mediainfo.SkipSegmentRange{{Category: "credits", StartSec: round1(bestStart), EndSec: round1(durationSec)}}
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
