package engine

import (
	"math"
	"strconv"
)

// TranscodeRuntime carries the resolved ffmpeg/ffprobe paths + tunables so
// each session can decide whether to passthrough or pipe through ffmpeg.
type TranscodeRuntime struct {
	FFmpegPath   string
	FFprobePath  string
	HWAccel      HWAccel
	Preset       string
	VideoBitrate string
	AudioBitrate string
	MaxHeight    int
	// Disabled forces passthrough for every file even when codecs are not
	// browser-friendly. Useful when the user explicitly turns transcoding
	// off in config.
	Disabled bool
	// TonemapHDR enables HDR→SDR tonemapping of HDR sources during transcode.
	// Set only when the ffmpeg build has zscale (FFmpegSupportsZscale); without
	// it the tonemap filter would error and break playback, so it stays off.
	TonemapHDR bool
	// HasLibplacebo: the ffmpeg build has the libplacebo filter (GPU HDR tonemap).
	// Preferred over the zscale chain for HDR sources — one GPU pass, higher
	// quality, and present where zscale is missing.
	HasLibplacebo bool
	// HasScaleCuda: this host can run scale_cuda (CUDA device + filter). Lets an
	// NVENC downscale of an SDR source stay fully on the GPU (decode → scale_cuda
	// → h264_nvenc) instead of round-tripping each frame to the CPU for `scale=`.
	// Probed functionally (FFmpegSupportsScaleCuda); false ⇒ keep the CPU scale.
	HasScaleCuda bool
}

// qualityCap maps a session's Quality label to a (MaxHeight, VideoBitrate)
// pair. An empty label or "original" returns zero-values, signalling "no
// override" to the caller.
type qualityCap struct {
	MaxHeight    int
	VideoBitrate string // ffmpeg -b:v string, e.g. "3500k"
}

func resolveQualityCap(label string) qualityCap {
	switch label {
	case "2160p":
		return qualityCap{MaxHeight: 2160, VideoBitrate: "25000k"}
	case "1080p":
		return qualityCap{MaxHeight: 1080, VideoBitrate: "6000k"}
	case "720p":
		return qualityCap{MaxHeight: 720, VideoBitrate: "3500k"}
	case "480p":
		return qualityCap{MaxHeight: 480, VideoBitrate: "1500k"}
	default:
		// "original", "auto", "" → defer to config.
		return qualityCap{}
	}
}

// doubleBitrate returns an ffmpeg bitrate string with twice the value of the
// input ("6000k" → "12000k", "1.5M" → "3M", "5M" → "10M"). Used to size
// `-bufsize` at the standard 2× of `-maxrate` for capped-CRF/CQ rate control.
// An unparseable string falls back to the input unchanged (1× bufsize — the
// pre-CRF behaviour, safe just suboptimal). The doubled CPB stays far below
// every H.264 level's limit for the (level, maxrate) pairs this package emits
// (worst case: 1080p level 4.1 → 12000k bufsize vs 62500k allowed).
func doubleBitrate(b string) string {
	if b == "" {
		return b
	}
	num := b
	suffix := ""
	switch b[len(b)-1] {
	case 'k', 'K', 'm', 'M':
		num = b[:len(b)-1]
		suffix = string(b[len(b)-1])
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v <= 0 {
		return b
	}
	d := v * 2
	if d == math.Trunc(d) {
		return strconv.FormatFloat(d, 'f', 0, 64) + suffix
	}
	return strconv.FormatFloat(d, 'f', -1, 64) + suffix
}

// capForHeight returns the bitrate-cap pair appropriate for an effective
// output height. Used after clamping outputHeight to the source's resolution:
// asking ffmpeg for "2160p" bitrate (25 Mbps) on a 1080p source overshoots
// the H.264 level we derived from the EFFECTIVE height (4.0, max 20 Mbps) and
// makes libx264 refuse with "VBV bitrate > level limit". This helper picks
// the bitrate that matches the level libx264 will actually accept.
func capForHeight(height int) qualityCap {
	switch {
	case height <= 0:
		return qualityCap{}
	case height <= 480:
		return qualityCap{MaxHeight: 480, VideoBitrate: "1500k"}
	case height <= 720:
		return qualityCap{MaxHeight: 720, VideoBitrate: "3500k"}
	case height <= 1080:
		return qualityCap{MaxHeight: 1080, VideoBitrate: "6000k"}
	case height <= 1440:
		return qualityCap{MaxHeight: 1440, VideoBitrate: "12000k"}
	default:
		return qualityCap{MaxHeight: 2160, VideoBitrate: "25000k"}
	}
}
