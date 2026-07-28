package library

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

// This file holds the stats QUALITY pass — the ffprobe-backed resolution/codec/HDR
// breakdown. Split out of stats.go to keep each file single-responsibility and
// under the architectural line limit.

// computeQuality probes every video under the roots (concurrently, bounded by
// Workers) and buckets resolution/codec/HDR. A file with no usable mediainfo is
// counted as "unknown" and never aborts the pass. Confined to the configured
// roots.
func computeQuality(ctx context.Context, opts StatsOptions) QualityStats {
	q := QualityStats{ByResolution: map[string]int{}, ByCodec: map[string]int{}}
	probe := opts.Probe
	if probe == nil {
		probe = defaultProber(opts.FFprobePath)
	}

	files := collectVideoFiles(opts.Paths.roots())
	q.Total = len(files)
	if len(files) == 0 {
		return q
	}

	infos := probeAll(ctx, files, probe, resolveWorkers(opts.Workers), opts.OnProgress)
	for _, vi := range infos {
		tallyQuality(&q, vi)
	}
	return q
}

// tallyQuality folds one probed video (nil = unprobeable) into the breakdown.
func tallyQuality(q *QualityStats, vi *mediainfo.VideoInfo) {
	if vi == nil {
		q.ByResolution["unknown"]++
		q.ByCodec["unknown"]++
		q.Unknown++
		return
	}
	res := ResolveResolution(vi.Width, vi.Height)
	if res == "" {
		res = "SD"
	}
	q.ByResolution[res]++
	q.ByCodec[normalizeCodec(vi.Codec)]++
	if vi.HDR != "" {
		q.HDR++
	} else {
		q.SDR++
	}
}

// probeAll runs probe over files with a bounded worker pool (the scanner's shape),
// returning the per-file VideoInfo (nil for an unprobeable file). Order is not
// significant — the caller only tallies.
func probeAll(ctx context.Context, files []string, probe MediaProber, workers int, onProgress func(done, total int)) []*mediainfo.VideoInfo {
	var (
		mu    sync.Mutex
		out   = make([]*mediainfo.VideoInfo, 0, len(files))
		done  atomic.Int32
		total = len(files)
		sem   = make(chan struct{}, workers)
		wg    sync.WaitGroup
	)
	for _, fp := range files {
		select {
		case <-ctx.Done():
			return out // a cancelled stats pass just reports what it managed to probe
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			defer func() { <-sem }()
			vi, err := probe(ctx, path)
			mu.Lock()
			if err != nil {
				out = append(out, nil)
			} else {
				out = append(out, vi)
			}
			mu.Unlock()
			if onProgress != nil {
				onProgress(int(done.Add(1)), total)
			}
		}(fp)
	}
	wg.Wait()
	return out
}

// collectVideoFiles returns every real video file (>= floor) under the roots,
// de-duplicated by path (a download dir that is the parent of movies/tv would
// otherwise double-count). Confined to roots.
func collectVideoFiles(roots []string) []string {
	seen := map[string]bool{}
	var files []string
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !isVideoFile(path) {
				return nil
			}
			info, statErr := os.Stat(path)
			if statErr != nil || info.Size() < MinPlausibleVideoBytes {
				return nil
			}
			clean := filepath.Clean(path)
			if seen[clean] {
				return nil
			}
			seen[clean] = true
			files = append(files, clean)
			return nil
		})
	}
	return files
}

// defaultProber wraps mediainfo.ExtractMediaInfo. If ffprobe can't be resolved,
// every probe returns an error → every video is tallied as "unknown" (the pass
// still completes), so a runner without ffprobe degrades gracefully.
func defaultProber(ffprobePath string) MediaProber {
	resolved, err := mediainfo.ResolveFFprobe(ffprobePath)
	if err != nil {
		return func(context.Context, string) (*mediainfo.VideoInfo, error) {
			return nil, err
		}
	}
	return func(ctx context.Context, filePath string) (*mediainfo.VideoInfo, error) {
		mi, probeErr := mediainfo.ExtractMediaInfo(ctx, resolved, filePath)
		if probeErr != nil {
			return nil, probeErr
		}
		if mi == nil || mi.Video == nil {
			return nil, errNoVideoStream
		}
		return mi.Video, nil
	}
}

// errNoVideoStream marks a probe that ran but found no video stream — tallied as
// "unknown" like any other unprobeable file.
var errNoVideoStream = &probeError{"no video stream"}

type probeError struct{ msg string }

func (e *probeError) Error() string { return e.msg }

// normalizeCodec buckets a raw ffprobe codec name into a coarse family for the
// breakdown: h265 (hevc/x265/h265), h264 (avc/x264/h264), av1, "other" for any
// other non-empty codec, "unknown" for empty.
func normalizeCodec(codec string) string {
	switch codec {
	case "":
		return "unknown"
	case "hevc", "h265", "x265":
		return "h265"
	case "h264", "avc", "x264":
		return "h264"
	case "av1":
		return "av1"
	default:
		return "other"
	}
}

// resolveWorkers mirrors the scanner default (8) for an unset/invalid count.
func resolveWorkers(n int) int {
	if n <= 0 {
		return 8
	}
	return n
}
