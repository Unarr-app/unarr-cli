package engine

import (
	"bytes"
	"context"
	"log"
	"os/exec"
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

var (
	zscaleCacheMu sync.Mutex
	zscaleCache   = map[string]bool{}
)

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
