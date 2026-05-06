// Package streaming wraps ffmpeg for the WebRTC-streaming pipeline.
//
// The browser-side reproductor lives on torrentclaw.com and consumes
// fragmented MP4 (fMP4) chunks via Media Source Extensions (MSE). MSE is
// strict about codecs: H.264 / VP8 / VP9 / AV1 video + AAC / Opus / MP3
// audio + MP4 / WebM container. Anything else (HEVC/x265, MKV, EAC3, FLAC,
// 10-bit H.264, …) needs transcoding.
//
// The transcoder picks one of two paths per request:
//
//   - Direct play  — input is already MSE-compatible. Container is remuxed
//     to fragmented MP4 with the audio + video streams copied. Cheap:
//     ~no CPU, ~no memory.
//
//   - Transcode    — input is incompatible. Re-encode video to H.264
//     (libx264 sw / h264_nvenc / h264_qsv / h264_vaapi / h264_videotoolbox
//     depending on what the host supports) and audio to AAC. Expensive:
//     1× core for 1080p sw, ~free with HW accel.
package streaming

import (
	"github.com/torrentclaw/unarr/internal/library/mediainfo"
)

// browserVideoCodecs lists video codecs the player can render natively
// without transcoding. Names match ffprobe's `codec_name`.
var browserVideoCodecs = map[string]struct{}{
	"h264": {},
	"vp8":  {},
	"vp9":  {},
	"av1":  {},
}

// browserAudioCodecs lists audio codecs the player accepts natively.
var browserAudioCodecs = map[string]struct{}{
	"aac":  {},
	"opus": {},
	"mp3":  {},
}

// browserPixelFormats lists pixel formats MSE H.264 reliably decodes
// in-browser. 10-bit / 12-bit profiles are rejected because Safari + most
// Chromium versions software-decode them at 1-2 fps.
var browserPixelFormats = map[string]struct{}{
	"yuv420p":  {},
	"yuvj420p": {},
}

// CompatibilityReport explains why a file is or isn't direct-playable.
// Returned by AnalyzeCompatibility so the caller can show actionable
// feedback (e.g. "transcoding video: HEVC → H.264").
type CompatibilityReport struct {
	DirectPlay  bool
	VideoCompat bool
	AudioCompat bool
	Container   string // input container hint (best effort)
	VideoCodec  string
	AudioCodec  string
	PixelFormat string
	BitDepth    int
	Reasons     []string // human-readable list of mismatches; empty when DirectPlay
}

// AnalyzeCompatibility inspects a parsed mediainfo and decides whether the
// stream needs transcoding. It does NOT touch disk or run ffmpeg.
//
// Direct play requires ALL of:
//   - Video codec ∈ {h264, vp8, vp9, av1}
//   - Pixel format ∈ {yuv420p, yuvj420p}
//   - Bit depth ≤ 8
//   - Audio codec ∈ {aac, opus, mp3}
//
// First audio track wins for the compatibility decision; later tracks are
// repacked along with it. Container is intentionally ignored — even MKV
// carrying H.264 + AAC can be remuxed to fMP4 cheaply, so it's not worth
// failing direct-play on container alone.
func AnalyzeCompatibility(info *mediainfo.MediaInfo) CompatibilityReport {
	r := CompatibilityReport{}
	if info == nil || info.Video == nil {
		r.Reasons = append(r.Reasons, "missing video stream metadata")
		return r
	}

	r.VideoCodec = info.Video.Codec
	r.PixelFormat = pixelFormatFor(info.Video)
	r.BitDepth = info.Video.BitDepth

	_, vcOK := browserVideoCodecs[r.VideoCodec]
	r.VideoCompat = vcOK
	if !vcOK {
		r.Reasons = append(r.Reasons,
			"video codec "+r.VideoCodec+" not playable in browser")
	}
	if r.BitDepth > 8 {
		r.VideoCompat = false
		r.Reasons = append(r.Reasons, "video bit depth >8 (HDR / 10-bit)")
	}
	if r.PixelFormat != "" {
		if _, ok := browserPixelFormats[r.PixelFormat]; !ok {
			r.VideoCompat = false
			r.Reasons = append(r.Reasons,
				"pixel format "+r.PixelFormat+" not playable in browser")
		}
	}

	if len(info.Audio) > 0 {
		r.AudioCodec = info.Audio[0].Codec
		_, acOK := browserAudioCodecs[r.AudioCodec]
		r.AudioCompat = acOK
		if !acOK {
			r.Reasons = append(r.Reasons,
				"audio codec "+r.AudioCodec+" not playable in browser")
		}
	} else {
		// No audio track — direct play allowed for video-only streams.
		r.AudioCompat = true
	}

	r.DirectPlay = r.VideoCompat && r.AudioCompat
	return r
}

// pixelFormatFor returns a best-effort pixel format string for a VideoInfo.
// mediainfo doesn't carry pix_fmt explicitly today, so we infer from the
// HDR flag: HDR streams are 10-bit yuv420p10le (incompatible by definition)
// while everything else is assumed yuv420p.
//
// Once mediainfo grows a PixFmt field we replace this heuristic with the
// raw value.
func pixelFormatFor(v *mediainfo.VideoInfo) string {
	if v.HDR != "" || v.BitDepth >= 10 {
		return "yuv420p10le"
	}
	return "yuv420p"
}
