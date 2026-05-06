package streaming

import (
	"fmt"
	"strconv"
	"time"
)

// StreamOptions controls a single transcode/remux invocation.
type StreamOptions struct {
	// Quality caps the output resolution and bitrate when transcoding.
	// Direct play ignores it (the source bitrate wins). One of:
	// "2160p", "1080p", "720p", "480p", "" (= "1080p").
	Quality string

	// StartOffset seeks the input N seconds in before transcoding. Useful
	// for resume / scrubbing. Zero means start from the beginning.
	StartOffset time.Duration

	// HW selects the hardware encoder. "" (or "none") means software libx264.
	HW HWAccel

	// AudioTrackIndex selects which audio track to keep (0-based, before
	// the video stream is excluded). Zero is the default track.
	AudioTrackIndex int
}

// QualityProfile maps a Quality label to encoder constraints.
type QualityProfile struct {
	Label        string // "1080p"
	MaxHeight    int    // 1080
	VideoBitrate int    // bits/s for libx264 -b:v
	AudioBitrate int    // bits/s for AAC
}

// qualityProfiles is the full ladder. We default to 1080p when unset.
var qualityProfiles = map[string]QualityProfile{
	"2160p": {Label: "2160p", MaxHeight: 2160, VideoBitrate: 25_000_000, AudioBitrate: 192_000},
	"1080p": {Label: "1080p", MaxHeight: 1080, VideoBitrate: 6_000_000, AudioBitrate: 160_000},
	"720p":  {Label: "720p", MaxHeight: 720, VideoBitrate: 3_500_000, AudioBitrate: 128_000},
	"480p":  {Label: "480p", MaxHeight: 480, VideoBitrate: 1_500_000, AudioBitrate: 96_000},
}

// ResolveQuality returns the QualityProfile for a label, falling back to
// 1080p when the label is empty / unknown.
func ResolveQuality(label string) QualityProfile {
	if p, ok := qualityProfiles[label]; ok {
		return p
	}
	return qualityProfiles["1080p"]
}

// fragmentedMP4Movflags are the magic flags MSE needs to consume an
// ffmpeg pipe as it's produced — avoids the moov atom being written at the
// end of the file (which would force buffering the whole stream).
const fragmentedMP4Movflags = "frag_keyframe+empty_moov+default_base_moof"

// BuildFFmpegArgs returns the argv (without the binary itself) for
// ffmpeg given the input file, stream options, and a compatibility report.
//
// Two recipes:
//
//   - Direct play: -c copy on every selected stream + remux to fMP4.
//   - Transcode:   re-encode video (libx264 / hwaccel) + audio (aac).
//
// The result writes fMP4 fragments to stdout (`pipe:1`) so the HTTP
// handler can stream them directly to the browser without touching disk.
func BuildFFmpegArgs(inputPath string, report CompatibilityReport, opts StreamOptions) []string {
	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-nostdin",
	}

	if opts.HW.HasDecoder() {
		args = append(args, opts.HW.DecoderArgs()...)
	}

	if opts.StartOffset > 0 {
		args = append(args, "-ss", formatDuration(opts.StartOffset))
	}

	args = append(args, "-i", inputPath)

	// Map first video + selected audio. Drop subtitles (browser handles
	// them out-of-band; baking them in is a Phase 4.x decision).
	args = append(args,
		"-map", "0:v:0",
		"-map", fmt.Sprintf("0:a:%d?", opts.AudioTrackIndex),
	)

	if report.DirectPlay {
		// Cheap path: copy streams, just remux container.
		args = append(args, "-c", "copy")
	} else {
		// Transcode path: pick encoder per HW.
		profile := ResolveQuality(opts.Quality)
		args = append(args, transcodeArgs(profile, opts.HW)...)
	}

	args = append(args,
		"-movflags", fragmentedMP4Movflags,
		"-f", "mp4",
		"pipe:1",
	)
	return args
}

// transcodeArgs returns the encoder + bitrate flags. Keeps the function
// flat so the BuildFFmpegArgs reader can scan the recipe top to bottom.
func transcodeArgs(profile QualityProfile, hw HWAccel) []string {
	args := []string{}

	// Video encoder.
	args = append(args, "-c:v", hw.VideoEncoder())

	// Scale filter caps the long edge to MaxHeight, preserving aspect.
	// `force_original_aspect_ratio=decrease` keeps it ≤ MaxHeight when
	// the source is taller and leaves smaller sources untouched. The
	// `force_divisible_by=2` keeps libx264 happy.
	scale := fmt.Sprintf(
		"scale=-2:%d:force_original_aspect_ratio=decrease:force_divisible_by=2",
		profile.MaxHeight,
	)
	if hw == HWAccelVAAPI {
		// VAAPI needs frames in the GPU surface, scaling is done with
		// scale_vaapi. We still upload via format=nv12.
		scale = fmt.Sprintf("format=nv12,hwupload,scale_vaapi=-2:%d", profile.MaxHeight)
	}
	args = append(args, "-vf", scale)

	// Bitrate ceiling (variable bitrate with 2× burst).
	args = append(args,
		"-b:v", strconv.Itoa(profile.VideoBitrate),
		"-maxrate", strconv.Itoa(profile.VideoBitrate*2),
		"-bufsize", strconv.Itoa(profile.VideoBitrate*4),
	)

	// SW-only: tune for low latency + don't waste cycles on the deepest
	// preset when we're feeding live playback.
	if hw == HWAccelNone || hw == HWAccelUnset {
		args = append(args,
			"-preset", "veryfast",
			"-tune", "zerolatency",
		)
	}

	// Force yuv420p so MSE reliably plays the result (some libx264
	// configurations otherwise emit yuv422p for SD content).
	args = append(args, "-pix_fmt", "yuv420p")

	// Audio: re-encode to AAC stereo. Mono / 5.1 sources are downmixed.
	args = append(args,
		"-c:a", "aac",
		"-b:a", strconv.Itoa(profile.AudioBitrate),
		"-ac", "2",
	)

	return args
}

// formatDuration prints a Go Duration as ffmpeg's `-ss HH:MM:SS.mmm`.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := int(d / time.Hour)
	d -= time.Duration(h) * time.Hour
	m := int(d / time.Minute)
	d -= time.Duration(m) * time.Minute
	s := float64(d) / float64(time.Second)
	return fmt.Sprintf("%02d:%02d:%06.3f", h, m, s)
}
