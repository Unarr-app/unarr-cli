package engine

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
