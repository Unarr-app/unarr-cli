package streaming

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// HWAccel identifies which hardware encoder family the host can use.
type HWAccel string

const (
	HWAccelUnset        HWAccel = ""
	HWAccelNone         HWAccel = "none"         // explicit software libx264
	HWAccelNVENC        HWAccel = "nvenc"        // NVIDIA GPUs
	HWAccelQSV          HWAccel = "qsv"          // Intel Quick Sync (Linux/Win)
	HWAccelVAAPI        HWAccel = "vaapi"        // Intel/AMD GPUs on Linux
	HWAccelVideoToolbox HWAccel = "videotoolbox" // macOS native
)

// VideoEncoder returns the ffmpeg `-c:v` argument for this accelerator.
func (h HWAccel) VideoEncoder() string {
	switch h {
	case HWAccelNVENC:
		return "h264_nvenc"
	case HWAccelQSV:
		return "h264_qsv"
	case HWAccelVAAPI:
		return "h264_vaapi"
	case HWAccelVideoToolbox:
		return "h264_videotoolbox"
	default:
		return "libx264"
	}
}

// HasDecoder reports whether the accelerator also supports HW decode.
// We always feed encoders software-decoded frames except for VAAPI where
// the GPU pipeline expects HW-decoded surfaces end-to-end.
func (h HWAccel) HasDecoder() bool {
	return h == HWAccelVAAPI
}

// DecoderArgs returns the ffmpeg flags that enable HW decode for this
// accelerator. Only meaningful when HasDecoder() == true.
func (h HWAccel) DecoderArgs() []string {
	if h == HWAccelVAAPI {
		return []string{
			"-hwaccel", "vaapi",
			"-hwaccel_device", "/dev/dri/renderD128",
			"-hwaccel_output_format", "vaapi",
		}
	}
	return nil
}

// detectedHWAccel caches the result of DetectHWAccel so we don't fork
// ffmpeg on every transcode request.
var (
	detectedHWAccelOnce sync.Once
	detectedHWAccel     HWAccel
)

// DetectHWAccel asks ffmpeg what encoders it supports and returns the
// best available. Result is cached for the process lifetime — callers
// should construct the Transcoder once and reuse it.
//
// Detection order (best perf → fallback):
//  1. NVENC      (NVIDIA GPU + CUDA driver)
//  2. QSV        (Intel iGPU/dGPU + libmfx/intel-media-driver)
//  3. VAAPI      (Linux Intel/AMD via /dev/dri)
//  4. VideoToolbox (macOS only)
//  5. None       (fallback to libx264 software)
func DetectHWAccel(ctx context.Context, ffmpegPath string) HWAccel {
	detectedHWAccelOnce.Do(func() {
		detectedHWAccel = doDetectHWAccel(ctx, ffmpegPath)
	})
	return detectedHWAccel
}

// ResetHWAccelCache forces the next DetectHWAccel call to re-probe.
// Intended for tests.
func ResetHWAccelCache() {
	detectedHWAccelOnce = sync.Once{}
	detectedHWAccel = HWAccelUnset
}

func doDetectHWAccel(ctx context.Context, ffmpegPath string) HWAccel {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
	}

	// macOS videotoolbox is reliable enough that we don't bother probing
	// — every Apple Silicon Mac has it; Intel Macs since 10.13 do too.
	if runtime.GOOS == "darwin" {
		if encoderAvailable(ctx, ffmpegPath, "h264_videotoolbox") {
			return HWAccelVideoToolbox
		}
	}

	for _, candidate := range []struct {
		Name    HWAccel
		Encoder string
	}{
		{HWAccelNVENC, "h264_nvenc"},
		{HWAccelQSV, "h264_qsv"},
		{HWAccelVAAPI, "h264_vaapi"},
	} {
		if encoderAvailable(ctx, ffmpegPath, candidate.Encoder) {
			return candidate.Name
		}
	}

	return HWAccelNone
}

// encoderAvailable returns true when `ffmpeg -hide_banner -encoders`
// lists the named encoder.
//
// Note: this only verifies ffmpeg was COMPILED with the encoder. It does
// NOT guarantee the host hardware works at runtime — some users will see
// libx264 fall back at the first failed encode. That's OK; the worst
// case is a one-time slow request.
func encoderAvailable(ctx context.Context, ffmpegPath, encoder string) bool {
	cmd := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-encoders")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		// `-encoders` output looks like:
		//   V..... libx264              libx264 H.264 / AVC / MPEG-4 AVC
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == encoder {
			return true
		}
	}
	return false
}
