package engine

import (
	"context"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Hardware downscale filter probes (F4). Mirror the libplacebo probe in
// tonemap.go: presence in `ffmpeg -filters` does NOT prove the filter RUNS —
// scale_cuda needs a working CUDA runtime + device, which the prod debian-slim
// image may lack even with the filter compiled in. So we run the real filter on
// one synthetic frame and require a clean exit, cached per binary.

var (
	scaleCudaCacheMu sync.Mutex
	scaleCudaCache   = map[string]bool{}
)

// FFmpegSupportsScaleCuda reports whether this host can ACTUALLY run scale_cuda
// — a working CUDA device + the filter compiled in. Used to keep an NVENC
// downscale fully on the GPU (decode → scale_cuda → h264_nvenc) instead of
// round-tripping each frame to the CPU for `scale=`, which is the wall on modest
// GPUs. Fails closed: any error → false → the caller keeps the CPU-scale path
// (no regression, just no speedup). Cached per path EXCEPT a context timeout,
// which is transient (a busy box) and must not pin the slow path for the run.
func FFmpegSupportsScaleCuda(ffmpegPath string) bool {
	if ffmpegPath == "" {
		return false
	}
	scaleCudaCacheMu.Lock()
	if v, ok := scaleCudaCache[ffmpegPath]; ok {
		scaleCudaCacheMu.Unlock()
		return v
	}
	scaleCudaCacheMu.Unlock()

	// 10 s: first-run CUDA device creation + filter init can take a beat on a
	// cold/busy box. Probe the WORST-CASE real input: a 10-bit (p010) surface
	// scaled down to 8-bit yuv420p. Most 4K SDR HEVC is Main10, so the gated
	// path routinely hands scale_cuda a 10-bit frame; an 8-bit-only probe would
	// pass on a host whose scale_cuda can't do the 10→8-bit conversion, and the
	// real session would then fail with no CPU fallback. testsrc2 is CPU-side,
	// so format=p010le + hwupload_cuda stands in for a hevc_cuda Main10 decode.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-nostats",
		"-init_hw_device", "cuda=cu:0", "-filter_hw_device", "cu",
		"-f", "lavfi", "-i", "testsrc2=size=256x256:rate=1:duration=1",
		"-vf", "format=p010le,hwupload_cuda,scale_cuda=64:64:format=yuv420p,hwdownload,format=yuv420p",
		"-frames:v", "1", "-f", "null", "-",
	).CombinedOutput()
	supported := err == nil

	// Cache a stable yes/no, but not a transient deadline (see libplacebo probe).
	if supported || ctx.Err() != context.DeadlineExceeded {
		scaleCudaCacheMu.Lock()
		scaleCudaCache[ffmpegPath] = supported
		scaleCudaCacheMu.Unlock()
	}
	if supported {
		log.Printf("[hwscale] ffmpeg scale_cuda works — NVENC SDR downscales stay on the GPU (no CPU round-trip)")
	} else {
		detail := strings.TrimSpace(lastLine(out))
		if detail == "" {
			detail = err.Error()
		}
		log.Printf("[hwscale] ffmpeg scale_cuda unavailable — NVENC keeps the CPU scale path: %v", detail)
	}
	return supported
}
