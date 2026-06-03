package engine

import (
	"context"
	"log"
	"os/exec"
	"strconv"
	"time"
)

// benchmarkRung is a candidate transcode-height ceiling plus the 16:9 frame
// size used to measure whether a software encoder sustains it.
type benchmarkRung struct {
	height int
	width  int
}

// softwareBenchmarkRungs are tested high→low. The frame sizes match the real
// streaming output tiers; the H.264 level / macroblock math in hls.go is
// independent of what we measure here.
var softwareBenchmarkRungs = []benchmarkRung{
	{height: 1080, width: 1920},
	{height: 720, width: 1280},
	{height: 480, width: 854},
}

// realtimeMarginSoftware is how much faster than realtime a synthetic encode
// must run before we call a rung "sustainable". 2.0× (not 1.5×) because the
// benchmark measures ONLY the encode of a low-entropy synthetic source and
// must cover two costs it never sees: (a) decoding the real source — software
// HEVC / 10-bit decode can rival the encode cost on its own — and (b) real
// content (film grain, motion) being far busier than testsrc2 for x264's
// rate-control + motion estimation. Erring high routes a borderline box's
// oversized sources to an external player (which works) instead of a
// stuttering transcode (which is the failure we're preventing).
const realtimeMarginSoftware = 2.0

// benchmarkClipSeconds is the synthetic clip length. Short enough that a
// capable host finishes the 1080p rung in well under a second, long enough to
// average out process spin-up.
const benchmarkClipSeconds = 3

// BenchmarkMaxTranscodeHeight returns the largest output height this host can
// software-transcode in real time, one of {1080,720,480}. Hardware encoders
// return 2160 WITHOUT benchmarking — NVENC/QSV/VAAPI/VideoToolbox all sustain
// 4K and a probe would only add startup latency.
//
// The point is the weak end. A low-power NAS or an old CPU can be
// ffmpeg-capable yet unable to keep up with a 1080p software encode, so the
// historical static 1080 ceiling makes the web side attempt a transcode that
// stutters. Measuring real throughput lets decideStreamPlan route oversized
// sources to an external player instead. Floors at 480: a box that can't
// sustain even that is barely functional, and 480-or-smaller sources transcode
// cheaply regardless — anything larger is already gated out by the 480 ceiling.
func BenchmarkMaxTranscodeHeight(ctx context.Context, ffmpegPath string, hw HWAccel) int {
	if hw != HWAccelNone {
		return 2160
	}
	if ffmpegPath == "" {
		return 1080 // no benchmark possible; keep the historical default
	}
	measuredAny := false
	for _, rung := range softwareBenchmarkRungs {
		factor, ok := measureEncodeRealtimeFactor(ctx, ffmpegPath, rung)
		if !ok {
			// Probe couldn't run (timeout / exec error) — try a lighter rung
			// rather than treat the failure as a measured "fast enough".
			log.Printf("[transcode] encode benchmark: %dp probe failed — trying lower", rung.height)
			continue
		}
		measuredAny = true
		if factor >= realtimeMarginSoftware {
			log.Printf("[transcode] encode benchmark: software ceiling %dp (%.1f× realtime)", rung.height, factor)
			return rung.height
		}
		log.Printf("[transcode] encode benchmark: %dp only %.1f× realtime (<%.1f×) — trying lower", rung.height, factor, realtimeMarginSoftware)
	}
	if !measuredAny {
		// No rung produced a measurement at all — the benchmark infrastructure
		// failed (missing lavfi/testsrc2, ffmpeg wedged), NOT a slow host. Don't
		// punish a possibly-capable box by flooring at 480; keep the historical
		// default so behaviour is no worse than before the benchmark existed.
		log.Printf("[transcode] encode benchmark: no rung could be measured (lavfi/ffmpeg issue) — keeping default 1080 ceiling")
		return 1080
	}
	log.Printf("[transcode] encode benchmark: host can't sustain 480p software encode — flooring ceiling at 480 (oversized sources route to external)")
	return 480
}

// measureEncodeRealtimeFactor encodes benchmarkClipSeconds of synthetic video
// at the rung's resolution using the real streaming encoder settings (libx264
// superfast, no B-frames) to /dev/null and returns clipDuration/wallTime — the
// realtime factor. ok=false when the probe couldn't run, so the caller skips
// rather than treating the failure as a fast result. Each probe is bounded so
// a wedged ffmpeg can't stall daemon startup.
func measureEncodeRealtimeFactor(ctx context.Context, ffmpegPath string, rung benchmarkRung) (float64, bool) {
	// A 3 s superfast encode that takes longer than 6 s is <0.5× realtime —
	// already far below the 2.0× bar — so capping here only kills genuinely
	// hopeless rungs early and bounds worst-case startup blocking (3 rungs ×
	// 6 s = 18 s) since this runs synchronously before the agent registers.
	bctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	size := strconv.Itoa(rung.width) + "x" + strconv.Itoa(rung.height)
	args := []string{
		"-hide_banner", "-nostats", "-loglevel", "error",
		"-f", "lavfi",
		"-i", "testsrc2=size=" + size + ":rate=24:duration=" + strconv.Itoa(benchmarkClipSeconds),
		"-c:v", "libx264", "-preset", "superfast", "-threads", "0",
		"-bf", "0", "-sc_threshold", "0",
		"-f", "null", "-",
	}
	start := time.Now()
	err := exec.CommandContext(bctx, ffmpegPath, args...).Run()
	elapsed := time.Since(start)
	if err != nil || elapsed <= 0 {
		return 0, false
	}
	return float64(benchmarkClipSeconds) / elapsed.Seconds(), true
}
