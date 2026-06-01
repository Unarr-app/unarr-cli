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

// HWAccelDiagnostic bundles what we know about the host's ffmpeg + HW encode
// capabilities so the daemon can log a single coherent line at startup and the
// web side can surface "this agent is software-only" without re-running probes.
type HWAccelDiagnostic struct {
	Pick          HWAccel  // backend selected by DetectHWAccel
	FFmpegPath    string   // resolved ffmpeg binary
	FFmpegVersion string   // first line of `ffmpeg -version` (e.g. "ffmpeg version 6.1.1")
	Encoders      []string // HW + libsvtav1/libvpx9-class encoders found in -encoders output
	Devices       []string // device files / drivers detected at probe time
}

// DetectHWAccelDiagnostic returns the full diagnostic picture for the host's
// transcode pipeline. Unlike DetectHWAccel, this is NOT cached — callers pay
// for an ffmpeg subprocess on each call (one `-encoders`, one `-version`).
// Daemon startup is the natural caller; per-session lookups should keep using
// DetectHWAccel (cached) and only re-probe diagnostics if the user runs an
// explicit doctor command.
func DetectHWAccelDiagnostic(ctx context.Context, ffmpegPath string) HWAccelDiagnostic {
	d := HWAccelDiagnostic{Pick: HWAccelNone, FFmpegPath: ffmpegPath}
	if ffmpegPath == "" {
		return d
	}
	d.FFmpegVersion = ffmpegVersionLine(ctx, ffmpegPath)
	encoders := listFFmpegEncoders(ctx, ffmpegPath)
	for _, name := range hwEncoderNames {
		if strings.Contains(encoders, name) {
			d.Encoders = append(d.Encoders, name)
		}
	}
	// Device-file checks mirror the picks below so the log line tells the
	// reader why a present encoder might still have been rejected (e.g. NVENC
	// compiled in but /dev/nvidia0 missing inside a container).
	if fileExists("/dev/nvidia0") {
		d.Devices = append(d.Devices, "/dev/nvidia0")
	}
	if fileExists("/dev/dri/renderD128") {
		d.Devices = append(d.Devices, "/dev/dri/renderD128")
	}
	if hasNvidiaDriver() {
		d.Devices = append(d.Devices, "nvidia-smi")
	}
	d.Pick = DetectHWAccel(ctx, ffmpegPath)
	return d
}

// LogLine returns a one-line human-readable summary of the diagnostic,
// suitable for daemon startup output. Format:
//
//	"[transcode] ffmpeg 6.1.1 at /usr/bin/ffmpeg, HW=nvenc (h264_nvenc), devices=/dev/nvidia0,nvidia-smi"
//	"[transcode] ffmpeg 6.1.1 at /home/linuxbrew/.../ffmpeg, HW=none (software libx264) — no HW encoders compiled in"
func (d HWAccelDiagnostic) LogLine() string {
	var b strings.Builder
	b.WriteString("[transcode] ")
	if d.FFmpegVersion != "" {
		b.WriteString(d.FFmpegVersion)
	} else {
		b.WriteString("ffmpeg")
	}
	if d.FFmpegPath != "" {
		b.WriteString(" at ")
		b.WriteString(d.FFmpegPath)
	}
	b.WriteString(", HW=")
	b.WriteString(string(d.Pick))
	if d.Pick == HWAccelNone {
		if len(d.Encoders) == 0 {
			b.WriteString(" (software libx264) — no HW encoders compiled in")
		} else {
			b.WriteString(" (software libx264) — encoders found but no matching device: ")
			b.WriteString(strings.Join(d.Encoders, ","))
		}
	} else {
		b.WriteString(" (")
		b.WriteString(d.Pick.FFmpegVideoCodec("h264"))
		b.WriteString(")")
		if len(d.Devices) > 0 {
			b.WriteString(", devices=")
			b.WriteString(strings.Join(d.Devices, ","))
		}
	}
	return b.String()
}

// hwEncoderNames lists the HW-accelerated encoders we care about for the
// startup log. Kept in lookup order so the output reads predictably across
// hosts.
var hwEncoderNames = []string{
	"h264_nvenc", "hevc_nvenc",
	"h264_qsv", "hevc_qsv",
	"h264_vaapi", "hevc_vaapi",
	"h264_videotoolbox", "hevc_videotoolbox",
}

// ffmpegVersionLine extracts the "ffmpeg version X.Y.Z" prefix from
// `ffmpeg -version`. Bounded to avoid hanging the daemon on a misbehaving
// binary.
func ffmpegVersionLine(ctx context.Context, ffmpegPath string) string {
	cmd := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-version")
	out, err := cmd.CombinedOutput()
	if err != nil || len(out) == 0 {
		return ""
	}
	line, _, _ := strings.Cut(string(out), "\n")
	// "ffmpeg version 6.1.1-some-build-suffix Copyright..." → keep up to first
	// space after "version 6.x" to avoid spamming build flags into the log.
	if idx := strings.Index(line, "Copyright"); idx > 0 {
		line = strings.TrimSpace(line[:idx])
	}
	return strings.TrimSpace(line)
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

// H264LevelForHeight returns the lowest H.264 profile level capable of
// encoding a stream at the given output pixel height. Each tier carries
// enough macroblock headroom to handle ANAMORPHIC content (up to ~2.4:1
// cinemascope) at 30 fps — a fixed 16:9 assumption used to silently bust
// the level on a 720p movie shot in 2.4:1 (1728×720 = 4860 MBs > 3.1's
// 3600 limit; libx264 logs "frame MB size > level limit" and emits a
// corrupt stream).
func H264LevelForHeight(height int) string {
	switch {
	case height <= 0:
		// Unknown source — pick a level that covers up to 4K so we never
		// re-introduce the silent-failure mode that motivated this helper.
		return "5.1"
	case height <= 480:
		return "3.1"
	case height <= 720:
		// 4.0 instead of 3.1: covers 720p anamorphic (e.g. 1728×720) +
		// MB rate up to 245k/s (3.1 caps at 108k/s — broken at 24 fps).
		return "4.0"
	case height <= 1080:
		// 4.1 instead of 4.0: covers 1080p anamorphic + 30 fps (~245k MBs/s).
		return "4.1"
	case height <= 1440:
		return "5.0"
	case height <= 2160:
		return "5.1"
	default:
		// 4K @ 60 fps and 8K all fall under 6.x.
		return "6.0"
	}
}

// h264LevelRank orders level strings so callers can pick the higher of two.
var h264LevelRank = map[string]int{
	"3.0": 30, "3.1": 31, "3.2": 32,
	"4.0": 40, "4.1": 41, "4.2": 42,
	"5.0": 50, "5.1": 51, "6.0": 60,
}

// levelForMacroblocks returns the lowest H.264 level whose MaxFS (frame size in
// macroblocks) covers `mbs`. The height-based H264LevelForHeight tier is correct
// for 16:9, but anamorphic content (2.39:1 cinemascope) scaled to a given height
// has a much wider frame: a 2.39:1 source downscaled to 1080 height becomes
// ~2586×1080 = 11016 MBs, which busts level 4.1's 8192-MB MaxFS. ffmpeg then
// fails the encode — libx264 with "frame MB size > level limit", h264_nvenc with
// "InitializeEncoder failed: invalid param (8): Invalid Level" — and emits zero
// packets (the whole HLS session stalls at "preparando sesión"). MaxFS values
// from the H.264 spec, Table A-1.
func levelForMacroblocks(mbs int) string {
	switch {
	case mbs <= 1620:
		return "3.0"
	case mbs <= 3600:
		return "3.1"
	case mbs <= 5120:
		return "3.2"
	case mbs <= 8192: // levels 4.0 and 4.1 share MaxFS 8192; pick 4.1 for headroom
		return "4.1"
	case mbs <= 8704:
		return "4.2"
	case mbs <= 22080:
		return "5.0"
	case mbs <= 36864:
		return "5.1"
	default:
		return "6.0"
	}
}

// H264LevelForFrame returns the lowest H.264 level that satisfies BOTH the
// height-derived tier (which carries macroblock-rate / fps headroom) and the
// actual frame's macroblock count (which catches anamorphic frames that are far
// wider than 16:9 at a given height). Use this instead of H264LevelForHeight
// wherever the output width is known — it never under-levels an ultra-wide
// frame, and for 16:9 content it returns exactly what H264LevelForHeight does.
func H264LevelForFrame(width, height int) string {
	byHeight := H264LevelForHeight(height)
	if width <= 0 || height <= 0 {
		return byHeight
	}
	// Macroblocks are 16×16; partial blocks at the edge still count (ceil).
	mbs := ((width + 15) / 16) * ((height + 15) / 16)
	byMB := levelForMacroblocks(mbs)
	if h264LevelRank[byMB] > h264LevelRank[byHeight] {
		return byMB
	}
	return byHeight
}
