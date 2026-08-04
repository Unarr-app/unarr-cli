package engine

import (
	"context"
	"log"
	"os/exec"
	"strconv"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
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

// Reasons an EncodeBenchmark reached its ceiling. They are the difference
// between "we measured this host" and "we could not measure it and fell back",
// which a renderer (unarr bench) and a cache reader (unarr doctor) both need to
// state honestly instead of presenting a default as a measurement.
const (
	EncodeReasonHardware     = "hwaccel"      // HW encoder present — no probe run
	EncodeReasonNoFFmpeg     = "no-ffmpeg"    // nothing to benchmark with
	EncodeReasonSustained    = "sustained"    // a rung cleared the realtime threshold
	EncodeReasonFloor        = "floor"        // measured, but not even 480p sustains
	EncodeReasonUnmeasurable = "unmeasurable" // every probe failed to run
)

// EncodeRungResult is one measured rung. Factor is meaningful only when
// Measured is true: a probe that could not run reports 0, which must never be
// read as "0× realtime, this host is hopeless".
type EncodeRungResult struct {
	Height   int     `json:"height"`
	Width    int     `json:"width"`
	Factor   float64 `json:"realtimeFactor"`
	Measured bool    `json:"measured"`
}

// EncodeBenchmark is the full outcome of the transcode-ceiling probe: the
// ceiling the daemon will advertise plus the evidence behind it. The daemon
// only needs Ceiling; `unarr bench` and the cache keep the rest so a user can
// see how close the host came to the next rung up.
type EncodeBenchmark struct {
	HWAccel   string             `json:"hwaccel"`
	Ceiling   int                `json:"maxTranscodeHeight"`
	Threshold float64            `json:"realtimeThreshold"`
	Reason    string             `json:"reason"`
	Rungs     []EncodeRungResult `json:"rungs,omitempty"`
}

// MeasureEncodeCeiling finds the largest output height this host can
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
//
// Deliberately silent: it is the shared measurement core behind the daemon's
// startup probe (which logs) and `unarr bench` (which renders a table), and a
// measurement function that writes to the log can only be used by one of them.
func MeasureEncodeCeiling(ctx context.Context, ffmpegPath string, hw HWAccel) EncodeBenchmark {
	res := EncodeBenchmark{HWAccel: string(hw), Threshold: realtimeMarginSoftware}
	if hw != HWAccelNone {
		res.Ceiling, res.Reason = 2160, EncodeReasonHardware
		return res
	}
	if ffmpegPath == "" {
		res.Ceiling, res.Reason = 1080, EncodeReasonNoFFmpeg // keep the historical default
		return res
	}
	measuredAny := false
	for _, rung := range softwareBenchmarkRungs {
		factor, ok := measureEncodeRealtimeFactor(ctx, ffmpegPath, rung)
		res.Rungs = append(res.Rungs, EncodeRungResult{
			Height: rung.height, Width: rung.width, Factor: factor, Measured: ok,
		})
		if !ok {
			// Probe couldn't run (timeout / exec error) — try a lighter rung
			// rather than treat the failure as a measured "fast enough".
			continue
		}
		measuredAny = true
		if factor >= realtimeMarginSoftware {
			res.Ceiling, res.Reason = rung.height, EncodeReasonSustained
			return res
		}
	}
	if !measuredAny {
		// No rung produced a measurement at all — the benchmark infrastructure
		// failed (missing lavfi/testsrc2, ffmpeg wedged), NOT a slow host. Don't
		// punish a possibly-capable box by flooring at 480; keep the historical
		// default so behaviour is no worse than before the benchmark existed.
		res.Ceiling, res.Reason = 1080, EncodeReasonUnmeasurable
		return res
	}
	res.Ceiling, res.Reason = 480, EncodeReasonFloor
	return res
}

// BenchmarkMaxTranscodeHeight is the daemon-startup entry point: it runs
// MeasureEncodeCeiling and narrates it to the daemon log, which is the only
// place a user can see WHY their agent advertised 720p. Returns just the
// ceiling because that is all the register payload carries.
func BenchmarkMaxTranscodeHeight(ctx context.Context, ffmpegPath string, hw HWAccel) int {
	res := MeasureEncodeCeiling(ctx, ffmpegPath, hw)
	for _, r := range res.Rungs {
		switch {
		case !r.Measured:
			log.Printf("[transcode] encode benchmark: %dp probe failed - trying lower", r.Height)
		case r.Factor >= res.Threshold:
			log.Printf("[transcode] encode benchmark: software ceiling %dp (%.1fx realtime)", r.Height, r.Factor)
		default:
			log.Printf("[transcode] encode benchmark: %dp only %.1fx realtime (<%.1fx) - trying lower", r.Height, r.Factor, res.Threshold)
		}
	}
	switch res.Reason {
	case EncodeReasonUnmeasurable:
		log.Printf("[transcode] encode benchmark: no rung could be measured (lavfi/ffmpeg issue) - keeping default 1080 ceiling")
	case EncodeReasonFloor:
		log.Printf("[transcode] encode benchmark: host can't sustain 480p software encode - flooring ceiling at 480 (oversized sources route to external)")
	}
	return res.Ceiling
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
	cmd := exec.CommandContext(bctx, ffmpegPath, args...)
	winproc.HideWindow(cmd)
	err := cmd.Run()
	elapsed := time.Since(start)
	if err != nil || elapsed <= 0 {
		return 0, false
	}
	return float64(benchmarkClipSeconds) / elapsed.Seconds(), true
}
