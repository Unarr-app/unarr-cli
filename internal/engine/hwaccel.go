package engine

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// HWAccel identifies a hardware-accelerated ffmpeg encoder family.
type HWAccel string

const (
	HWAccelNone         HWAccel = "none"
	HWAccelNVENC        HWAccel = "nvenc"        // NVIDIA — h264_nvenc / hevc_nvenc
	HWAccelQSV          HWAccel = "qsv"          // Intel Quick Sync — h264_qsv / hevc_qsv
	HWAccelVAAPI        HWAccel = "vaapi"        // Linux open-source — h264_vaapi / hevc_vaapi
	HWAccelVideoToolbox HWAccel = "videotoolbox" // macOS — h264_videotoolbox
)

var (
	hwOnce  sync.Once
	hwCache HWAccel
)

// DetectHWAccel returns the most capable hardware encoder available on this
// host, or HWAccelNone if software-only. Cached after first call — adding /
// removing a GPU at runtime is rare and the cost of probing isn't free.
func DetectHWAccel(ctx context.Context, ffmpegPath string) HWAccel {
	hwOnce.Do(func() {
		hwCache = detectHWAccelFresh(ctx, ffmpegPath)
	})
	return hwCache
}

// ResetHWAccelCache clears the singleton — only used in tests.
func ResetHWAccelCache() {
	hwOnce = sync.Once{}
	hwCache = ""
}

func detectHWAccelFresh(ctx context.Context, ffmpegPath string) HWAccel {
	if ffmpegPath == "" {
		return HWAccelNone
	}
	encoders := listFFmpegEncoders(ctx, ffmpegPath)
	if encoders == "" {
		return HWAccelNone
	}

	// macOS — VideoToolbox is always available on Apple Silicon + recent Intel.
	if runtime.GOOS == "darwin" && strings.Contains(encoders, "h264_videotoolbox") {
		return HWAccelVideoToolbox
	}

	// NVIDIA — encoder presence + a CUDA-capable device. We rely on the
	// existence of the device file rather than running nvidia-smi to keep
	// startup quick on hosts without nvidia tooling.
	if strings.Contains(encoders, "h264_nvenc") &&
		(fileExists("/dev/nvidia0") || hasNvidiaDriver()) {
		return HWAccelNVENC
	}

	// Intel Quick Sync — needs /dev/dri (also used by VA-API). Distinguish by
	// checking whether the QSV-specific encoder is built in.
	if strings.Contains(encoders, "h264_qsv") && fileExists("/dev/dri/renderD128") {
		return HWAccelQSV
	}

	// Linux generic VA-API — works on Intel + AMD with mesa drivers.
	if strings.Contains(encoders, "h264_vaapi") && fileExists("/dev/dri/renderD128") {
		return HWAccelVAAPI
	}

	return HWAccelNone
}

func listFFmpegEncoders(ctx context.Context, ffmpegPath string) string {
	cmd := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-encoders")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return string(out)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func hasNvidiaDriver() bool {
	// Cheap proxy — if the user has nvidia-smi on PATH they presumably also
	// have a working driver / runtime libraries.
	_, err := exec.LookPath("nvidia-smi")
	return err == nil
}

// FFmpegVideoCodec returns the encoder name to pass to `-c:v` for the
// requested HW accel + target (h264 or hevc).
func (h HWAccel) FFmpegVideoCodec(target string) string {
	target = strings.ToLower(target)
	switch h {
	case HWAccelNVENC:
		if target == "hevc" {
			return "hevc_nvenc"
		}
		return "h264_nvenc"
	case HWAccelQSV:
		if target == "hevc" {
			return "hevc_qsv"
		}
		return "h264_qsv"
	case HWAccelVAAPI:
		if target == "hevc" {
			return "hevc_vaapi"
		}
		return "h264_vaapi"
	case HWAccelVideoToolbox:
		if target == "hevc" {
			return "hevc_videotoolbox"
		}
		return "h264_videotoolbox"
	default:
		// Software fallback. libx264 ships with every ffmpeg build.
		return "libx264"
	}
}

// H264LevelForHeight returns the lowest H.264 profile level capable of encoding
// a stream at the given output pixel height (assumes ~16:9, ≤30 fps). The
// previous code used a fixed "4.0" which silently rejects anything above 1080p
// — libx264 logs "frame MB size > level limit" and emits a corrupt stream.
// Returning a tighter level on smaller outputs keeps player compatibility on
// older devices where the encoder can't auto-pick.
func H264LevelForHeight(height int) string {
	switch {
	case height <= 0:
		// Unknown source — pick a level that covers up to 4K so we never
		// re-introduce the silent-failure mode that motivated this helper.
		return "5.1"
	case height <= 480:
		return "3.0"
	case height <= 720:
		return "3.1"
	case height <= 1080:
		return "4.0"
	case height <= 1440:
		return "5.0"
	case height <= 2160:
		return "5.1"
	default:
		// 4K @ 60 fps and 8K all fall under 6.x.
		return "6.0"
	}
}
