package mediainfo

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Chromaprint-based shared-audio detection. Episodes of the same season share
// an identical intro (OP) and credits (ED) audio track; fingerprinting a window
// of each episode and finding the longest aligned low-hamming-distance region
// between two episodes localizes those segments. Clean-room implementation of
// the approach popularized by Jellyfin's Intro Skipper plugin.
//
// Fingerprint stream: chromaprint emits one uint32 per ~0.1238s of audio
// (11025 Hz mono, FFT 4096, 2/3 overlap → ~8.08 points/second).

const (
	// ChromaprintSampleDur is seconds of audio per fingerprint point.
	ChromaprintSampleDur = 0.1238
	// maxHammingBits: two points are "similar" when their XOR popcount is below this.
	maxHammingBits = 6
	// maxTimeSkipSec: gap tolerance when growing a contiguous similar region.
	maxTimeSkipSec = 3.5
)

// SkipSegmentRange is one detected skippable range inside a media file.
type SkipSegmentRange struct {
	Category string  `json:"category"` // "intro" | "credits"
	StartSec float64 `json:"startSec"`
	EndSec   float64 `json:"endSec"`
}

// FingerprintAudioWindow decodes [startSec, startSec+lengthSec] of the first
// audio track with ffmpeg and pipes the WAV into fpcalc -raw, returning the
// chromaprint point stream.
func FingerprintAudioWindow(ctx context.Context, ffmpegPath, fpcalcPath, mediaPath string, startSec, lengthSec float64) ([]uint32, error) {
	ff := exec.CommandContext(ctx, ffmpegPath,
		"-nostdin", "-loglevel", "error",
		"-ss", strconv.FormatFloat(startSec, 'f', 3, 64),
		"-i", mediaPath,
		"-t", strconv.FormatFloat(lengthSec, 'f', 3, 64),
		"-map", "0:a:0",
		"-ac", "2",
		"-f", "wav", "-",
	)
	fp := exec.CommandContext(ctx, fpcalcPath,
		"-raw", "-length", strconv.Itoa(int(lengthSec)), "-")

	pipe, err := ff.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg pipe: %w", err)
	}
	fp.Stdin = pipe
	var ffErr strings.Builder
	ff.Stderr = &ffErr

	if err := ff.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg start: %w", err)
	}
	out, err := fp.Output()
	// fpcalc stops reading once it has processed -length seconds and may exit
	// WITHOUT draining the last buffered bytes. Close our read end so ffmpeg
	// gets EPIPE and exits — otherwise it blocks forever on a full pipe whose
	// only remaining reader is us (caught live: 5-min ctx kills, per file).
	_ = pipe.Close()
	// Always reap ffmpeg; early pipe close makes it exit non-zero — fine as
	// long as fpcalc produced output.
	_ = ff.Wait()
	if err != nil {
		return nil, fmt.Errorf("fpcalc: %w (ffmpeg: %s)", err, strings.TrimSpace(ffErr.String()))
	}

	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "FINGERPRINT="); ok {
			parts := strings.Split(rest, ",")
			points := make([]uint32, 0, len(parts))
			for _, p := range parts {
				// fpcalc may print signed ints; parse wide and truncate.
				v, perr := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
				if perr != nil {
					return nil, fmt.Errorf("fpcalc output parse: %w", perr)
				}
				points = append(points, uint32(v))
			}
			if len(points) == 0 {
				return nil, fmt.Errorf("fpcalc produced an empty fingerprint")
			}
			return points, nil
		}
	}
	return nil, fmt.Errorf("no FINGERPRINT line in fpcalc output")
}

// SharedRegion is the longest aligned similar-audio region between two
// fingerprint streams, in seconds relative to each stream's start.
type SharedRegion struct {
	AStart, AEnd float64
	BStart, BEnd float64
	Duration     float64
}

// FindSharedRegion locates the longest contiguous region (bounded by
// minDur/maxDur seconds) where streams a and b carry near-identical audio at
// some alignment. Returns nil when no qualifying region exists.
func FindSharedRegion(a, b []uint32, minDur, maxDur float64) *SharedRegion {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	// Inverted index of b: point value → last index seen.
	indexB := make(map[uint32]int, len(b))
	for i, v := range b {
		indexB[v] = i
	}
	// Candidate alignments: exact value matches (±2 on the value tolerates
	// quantization noise between encodes).
	shifts := make(map[int]struct{})
	for i, v := range a {
		for d := -2; d <= 2; d++ {
			if j, ok := indexB[v+uint32(d)]; ok {
				shifts[j-i] = struct{}{}
			}
		}
	}

	minPoints := int(minDur / ChromaprintSampleDur)
	gapSec := float64(maxTimeSkipSec)
	gapPoints := int(gapSec / ChromaprintSampleDur)
	var best *SharedRegion

	for shift := range shifts {
		i0 := 0
		if shift < 0 {
			i0 = -shift
		}
		i1 := len(a)
		if len(b)-shift < i1 {
			i1 = len(b) - shift
		}
		if i1-i0 < minPoints {
			continue
		}
		runStart, prev := -1, -1
		flush := func(end int) {
			if runStart < 0 {
				return
			}
			dur := float64(end-runStart) * ChromaprintSampleDur
			if dur >= minDur && dur <= maxDur && (best == nil || dur > best.Duration) {
				best = &SharedRegion{
					AStart:   float64(runStart) * ChromaprintSampleDur,
					AEnd:     float64(end) * ChromaprintSampleDur,
					BStart:   float64(runStart+shift) * ChromaprintSampleDur,
					BEnd:     float64(end+shift) * ChromaprintSampleDur,
					Duration: dur,
				}
			}
		}
		for i := i0; i < i1; i++ {
			if bits.OnesCount32(a[i]^b[i+shift]) > maxHammingBits {
				continue
			}
			if prev >= 0 && i-prev > gapPoints {
				flush(prev)
				runStart = i
			} else if runStart < 0 {
				runStart = i
			}
			prev = i
		}
		flush(prev)
	}
	return best
}

// --- Black-frame credits detection (movies: no sibling episode to compare) ---

var blackframeRe = regexp.MustCompile(`frame:\d+\s+pblack:\d+\s+pts:\d+\s+t:([\d.]+)`)

// DetectBlackFrameRuns scans [startSec, startSec+lengthSec] with ffmpeg's
// blackframe filter and returns the timestamps (absolute seconds) of frames
// that are ≥minBlackPct black. Used to find the start of end credits in movies
// (classic credits roll on black).
func DetectBlackFrameRuns(ctx context.Context, ffmpegPath, mediaPath string, startSec, lengthSec float64, minBlackPct int) ([]float64, error) {
	// Keyframe-only decode: credits-on-black lasts minutes, so sampling one
	// frame every keyframe interval (~2-10s) finds the run at ~2% of the cost
	// of a full decode — the difference between seconds and minutes per 4K film.
	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-nostdin", "-loglevel", "info",
		"-skip_frame", "nokey",
		"-ss", strconv.FormatFloat(startSec, 'f', 3, 64),
		"-i", mediaPath,
		"-t", strconv.FormatFloat(lengthSec, 'f', 3, 64),
		"-an", "-sn",
		"-vf", fmt.Sprintf("blackframe=amount=%d:threshold=32", minBlackPct),
		"-f", "null", "-",
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg blackframe start: %w", err)
	}
	var times []float64
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if m := blackframeRe.FindStringSubmatch(sc.Text()); m != nil {
			if t, perr := strconv.ParseFloat(m[1], 64); perr == nil {
				times = append(times, startSec+t)
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("ffmpeg blackframe: %w", err)
	}
	return times, nil
}

// --- Sidecar cache for detected segments ---

// skipSegmentsSidecarVersion bumps when the detection algorithm changes enough
// that cached results should be recomputed.
const skipSegmentsSidecarVersion = 1

// SkipSegmentsSidecar is the cached detection result for one media file.
type SkipSegmentsSidecar struct {
	Version     int                `json:"version"`
	DurationSec float64            `json:"durationSec"`
	Segments    []SkipSegmentRange `json:"segments"` // empty = analyzed, nothing found
}

func skipSegmentsCachePath(mediaPath string) string {
	return filepath.Join(sidecarDir(mediaPath), filepath.Base(mediaPath)+".skipseg.json")
}

// ReadCachedSkipSegments returns the cached detection result for mediaPath if
// fresh (newer than the media file) and of the current algorithm version.
func ReadCachedSkipSegments(mediaPath string) (*SkipSegmentsSidecar, bool) {
	p := skipSegmentsCachePath(mediaPath)
	if !sidecarFresh(p, mediaPath) {
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	var sc SkipSegmentsSidecar
	if err := json.Unmarshal(data, &sc); err != nil || sc.Version != skipSegmentsSidecarVersion {
		return nil, false
	}
	return &sc, true
}

// WriteCachedSkipSegments persists a detection result next to the media file.
func WriteCachedSkipSegments(mediaPath string, durationSec float64, segs []SkipSegmentRange) error {
	if segs == nil {
		segs = []SkipSegmentRange{}
	}
	sc := SkipSegmentsSidecar{Version: skipSegmentsSidecarVersion, DurationSec: durationSec, Segments: segs}
	data, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	dir := sidecarDir(mediaPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(skipSegmentsCachePath(mediaPath), data, 0o644)
}
