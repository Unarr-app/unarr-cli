package engine

import (
	"bytes"
	"context"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// hdrTonemapChain is the ffmpeg filter segment that maps an HDR source
// (HDR10/HLG, or a Dolby Vision base layer) down to SDR BT.709: linearise the
// PQ/HLG signal, tonemap the highlights (hable), then re-encode to BT.709
// transfer/matrix/primaries in limited range. Without it an HDR source
// transcoded to an SDR encode keeps wide-gamut/PQ data the SDR player can't
// interpret, so colour looks washed-out / desaturated.
//
// Requires the zscale filter (libzimg) in the ffmpeg build — gate on
// FFmpegSupportsZscale. Trailing comma so it slots in front of the chain's
// `format=` stage. CPU filter: valid for every encoder here because the decode
// hwaccel intentionally leaves frames in system memory (see buildHLSFFmpegArgsAt).
//
// Tuned for HDR10/PQ (npl=100) and the common DV+HDR10 case. HLG and bare-DV
// (Profile 5, no PQ signalling) get an approximate mapping — zscale linearises
// from whatever transfer the stream declares — but the result is still clearly
// better than the untonemapped washed-out baseline. A per-transfer chain is a
// possible follow-up if HLG/DV-only sources become common.
const hdrTonemapChain = "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,"

// libplaceboTonemapFilter maps an HDR source to SDR BT.709 in a SINGLE GPU pass
// (Vulkan): tone-map the HDR curve, convert primaries/transfer/matrix to BT.709
// limited range, and output 8-bit yuv420p — so it REPLACES the zscale chain AND
// the trailing `format=yuv420p,setparams=bt709` (it does both). Higher quality
// and far cheaper than the CPU zscale chain, and the agent's ffmpeg has it where
// zscale is missing. It does NOT scale here — the CPU scale chain runs first
// (it owns the even-dimension rounding libx264/nvenc require). No trailing comma:
// it's the last filter in the chain.
const libplaceboTonemapFilter = "libplacebo=colorspace=bt709:color_primaries=bt709:color_trc=bt709:range=tv:format=yuv420p:tonemapping=bt.2390"

var (
	zscaleCacheMu sync.Mutex
	zscaleCache   = map[string]bool{}

	libplaceboCacheMu sync.Mutex
	libplaceboCache   = map[string]bool{}
)

// FFmpegSupportsLibplacebo reports whether this host can ACTUALLY run the
// libplacebo filter — not merely whether it is compiled in. libplacebo is a
// Vulkan filter, so it needs a working Vulkan device + ICD at runtime, which a
// presence check (`ffmpeg -filters`) does NOT prove: the prod agent image
// ships a BtbN GPL ffmpeg with libplacebo built in but no Vulkan runtime
// (debian-slim, no libvulkan1 / mesa-vulkan-drivers / nvidia ICD), so a
// presence check would flip this on and break HDR playback that previously
// tonemapped fine via zscale.
//
// So we run the real filter on one synthetic frame and require a clean exit:
// that forces Vulkan device creation + filtergraph negotiation (libplacebo
// auto-inserts the hwupload/hwdownload around itself). Pass → libplacebo works
// here; fail → fall back to the zscale chain. Cached per path — EXCEPT a
// context timeout, which is transient (a busy box during the startup warm) and
// must not pin HDR to zscale for the whole process. The probe is bounded so a
// wedged ffmpeg can't stall the first session.
func FFmpegSupportsLibplacebo(ffmpegPath string) bool {
	if ffmpegPath == "" {
		return false
	}
	libplaceboCacheMu.Lock()
	if v, ok := libplaceboCache[ffmpegPath]; ok {
		libplaceboCacheMu.Unlock()
		return v
	}
	libplaceboCacheMu.Unlock()

	// 10 s: first-run Vulkan device creation alone can take ~1 s ("Spent
	// ~1150ms creating vulkan device"), plus codec/filter init.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Run the EXACT filter we'd use, on a 1-frame synthetic source, discarding
	// output. testsrc2 is SDR so the tonemap is near-passthrough — the point is
	// to exercise Vulkan device init + the filter, not the mapping quality.
	out, err := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-nostats",
		"-f", "lavfi", "-i", "testsrc2=size=128x128:rate=1:duration=1",
		"-vf", libplaceboTonemapFilter, "-frames:v", "1", "-f", "null", "-",
	).CombinedOutput()
	supported := err == nil

	// Cache the result — but NOT a timeout. A clean non-zero exit (filter
	// absent, no Vulkan ICD) is a stable "no" worth remembering; a deadline is
	// transient (the box was busy, e.g. the startup warm racing the encode
	// benchmark) and caching it would force HDR onto the zscale CPU chain until
	// restart. Worst case a perpetually-loaded box re-probes per session — rare,
	// and it fails closed to zscale each time.
	if supported || ctx.Err() != context.DeadlineExceeded {
		libplaceboCacheMu.Lock()
		libplaceboCache[ffmpegPath] = supported
		libplaceboCacheMu.Unlock()
	}
	if supported {
		log.Printf("[tonemap] ffmpeg libplacebo works (Vulkan OK) — HDR sources tonemapped on the GPU (preferred)")
	} else {
		// On an exec/timeout failure the stderr tail is empty — surface err
		// itself so the log distinguishes "no Vulkan" from "ffmpeg never ran".
		detail := strings.TrimSpace(lastLine(out))
		if detail == "" {
			detail = err.Error()
		}
		log.Printf("[tonemap] ffmpeg libplacebo unavailable (no Vulkan runtime or filter absent) — HDR falls back to zscale/none: %v", detail)
	}
	return supported
}

// lastLine returns the last non-empty line of ffmpeg output — the actual error
// (e.g. "No VK_ICD..." / "Device creation failed") rather than the whole log.
func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// FFmpegSupportsZscale reports whether the ffmpeg binary at path was built with
// the zscale filter (libzimg), required for HDR→SDR tonemapping. Cached per
// path. A detection failure (binary missing, exec error) is treated as "no" so
// tonemapping is simply skipped — the source still plays, just without it.
func FFmpegSupportsZscale(ffmpegPath string) bool {
	if ffmpegPath == "" {
		return false
	}
	zscaleCacheMu.Lock()
	if v, ok := zscaleCache[ffmpegPath]; ok {
		zscaleCacheMu.Unlock()
		return v
	}
	zscaleCacheMu.Unlock()

	// Probe OUTSIDE the lock: `ffmpeg -filters` can take a beat, and holding the
	// mutex across it would stall a concurrent session start. Worst case two
	// cold callers probe the same binary at once — both write the same bool.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-filters").Output()
	supported := err == nil && bytes.Contains(out, []byte("zscale"))

	zscaleCacheMu.Lock()
	zscaleCache[ffmpegPath] = supported
	zscaleCacheMu.Unlock()
	if supported {
		log.Printf("[tonemap] ffmpeg has zscale — HDR sources will be tonemapped to SDR")
	} else {
		log.Printf("[tonemap] ffmpeg %q lacks zscale — HDR sources play without tonemapping (desaturated)", ffmpegPath)
	}
	return supported
}
