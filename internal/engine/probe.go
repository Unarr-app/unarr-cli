package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/torrentclaw/unarr/internal/library/mediainfo"
)

// StreamProbe summarises the codec / container shape of a file as it relates
// to the HLS streaming pipeline. It tells the transcoder whether bytes can
// be streamed as-is, just remuxed to fragmented MP4, or fully transcoded.
type StreamProbe struct {
	// VideoCodec lowercased — e.g. "h264", "hevc", "av1", "vp9", "mpeg4".
	VideoCodec string
	// AudioCodec lowercased — e.g. "aac", "ac3", "dts", "eac3", "opus".
	// Reflects the default/first audio track for legacy single-track callers.
	AudioCodec string
	// Width / Height of the primary video stream.
	Width  int
	Height int
	// BitDepth — 8, 10 or 12. 0 if unknown.
	BitDepth int
	// HDR signalling string ("HDR10" / "DV" / "HLG" / etc, or "" for SDR).
	HDR string
	// DurationSec is the file length, used to sanity-check seek targets.
	DurationSec float64
	// Container is the file extension lowercased (".mp4", ".mkv", ".avi").
	Container string
	// AudioTracks lists every audio stream in source order. Index in this
	// slice == ffmpeg `-map 0:a:N` index (where N starts at 0).
	AudioTracks []ProbeAudioTrack
	// SubtitleTracks lists every subtitle stream in source order. Index in
	// this slice == ffmpeg `-map 0:s:N` index.
	SubtitleTracks []ProbeSubtitleTrack
}

// ProbeAudioTrack is a slimmed AudioTrack view tied to ffmpeg stream index.
type ProbeAudioTrack struct {
	Index    int    // 0-based audio stream index (ffmpeg -map 0:a:Index)
	Lang     string // ISO 639-1
	Codec    string // lowercased
	Channels int
	Title    string
	Default  bool
}

// ProbeSubtitleTrack is a slimmed SubtitleTrack view tied to ffmpeg stream index.
// Codec discriminates text (srt/ass/webvtt → extract to WebVTT) vs bitmap
// (pgs/dvbsub → require burn-in).
type ProbeSubtitleTrack struct {
	Index  int    // 0-based EMBEDDED subtitle stream index (ffmpeg -map 0:s:Index). Unused when External.
	Lang   string // ISO 639-1
	Codec  string // lowercased — "subrip", "ass", "webvtt", "hdmv_pgs_subtitle", ...
	Title  string
	Forced bool
	// External marks a sidecar file (served via /sub?p=<Path>&i=-1) rather than
	// an embedded stream. Path is its absolute filesystem path (External only).
	External bool
	Path     string
}

// IsTextSubtitle reports whether a subtitle codec can be extracted to WebVTT
// without re-rendering. Bitmap subs (PGS, DVB) need burn-in.
func (s ProbeSubtitleTrack) IsTextSubtitle() bool {
	switch s.Codec {
	case "subrip", "srt", "ass", "ssa", "webvtt", "mov_text", "text":
		return true
	}
	return false
}

// TranscodeAction tells the streaming pipeline how to feed the file to
// the browser <video> element. The decision matrix is documented in the
// project plan (Fase 2.5 — Transcoding on-the-fly).
type TranscodeAction string

const (
	// ActionPassthrough — file is already browser-playable as-is. Stream the
	// raw bytes via ReadAt; no ffmpeg involved.
	ActionPassthrough TranscodeAction = "passthrough"
	// ActionRemux — codecs are browser-compatible but the container or moov
	// placement is not. Run ffmpeg with `-c copy -movflags frag_keyframe`.
	ActionRemux TranscodeAction = "remux"
	// ActionRemuxAudio — video is fine but audio needs a re-encode (AC3/DTS
	// → AAC). `-c:v copy -c:a aac`.
	ActionRemuxAudio TranscodeAction = "remux-audio"
	// ActionTranscodeVideo — full re-encode. Used for HEVC/AV1 and any
	// 10-bit content if the browser refuses the codec.
	ActionTranscodeVideo TranscodeAction = "transcode-video"
)

// ProbeFile runs ffprobe and returns a StreamProbe view of the file.
//
// Result is memoised by (path, mtime, size) for probeCacheTTL — repeat plays
// of the same file at the same quality (the HLS cache HIT path) skip ffprobe
// entirely. ffprobe on a 50 GB MKV can cost 1-3 s; first-segment latency
// shrinks by the same amount on the second play.
func ProbeFile(ctx context.Context, ffprobePath, filePath string) (*StreamProbe, error) {
	if cached, ok := lookupProbeCache(filePath); ok {
		return cached, nil
	}
	mi, err := mediainfo.ExtractMediaInfo(ctx, ffprobePath, filePath)
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	probe := &StreamProbe{Container: lowerExt(filePath)}
	if mi.Video != nil {
		probe.VideoCodec = strings.ToLower(mi.Video.Codec)
		probe.Width = mi.Video.Width
		probe.Height = mi.Video.Height
		probe.BitDepth = mi.Video.BitDepth
		probe.HDR = mi.Video.HDR
		probe.DurationSec = mi.Video.Duration
	}
	if len(mi.Audio) > 0 {
		// Default to the first track marked "Default", else the first track.
		picked := mi.Audio[0]
		for _, a := range mi.Audio {
			if a.Default {
				picked = a
				break
			}
		}
		probe.AudioCodec = strings.ToLower(picked.Codec)
		probe.AudioTracks = make([]ProbeAudioTrack, 0, len(mi.Audio))
		for i, a := range mi.Audio {
			probe.AudioTracks = append(probe.AudioTracks, ProbeAudioTrack{
				Index:    i,
				Lang:     a.Lang,
				Codec:    strings.ToLower(a.Codec),
				Channels: a.Channels,
				Title:    a.Title,
				Default:  a.Default,
			})
		}
	}
	if len(mi.Subtitles) > 0 {
		probe.SubtitleTracks = make([]ProbeSubtitleTrack, 0, len(mi.Subtitles))
		// Embedded streams come first (ffprobe order); external sidecars are
		// appended after. Count embedded separately so each embedded track's
		// Index is its true `0:s:N` value regardless of how many externals trail
		// it; externals get Index=-1 and address by Path instead.
		embeddedIdx := 0
		for _, s := range mi.Subtitles {
			t := ProbeSubtitleTrack{
				Lang:     s.Lang,
				Codec:    strings.ToLower(s.Codec),
				Title:    s.Title,
				Forced:   s.Forced,
				External: s.External,
				Path:     s.Path,
			}
			if s.External {
				t.Index = -1
			} else {
				t.Index = embeddedIdx
				embeddedIdx++
			}
			probe.SubtitleTracks = append(probe.SubtitleTracks, t)
		}
	}
	storeProbeCache(filePath, probe)
	return probe, nil
}

// DecideAction maps a probe to the transcoding action the streaming pipeline
// should take. Browsers consume MP4/h264+AAC natively; everything else needs
// some level of re-shaping.
func DecideAction(p *StreamProbe) TranscodeAction {
	if p == nil {
		return ActionPassthrough
	}
	video := p.VideoCodec
	audio := p.AudioCodec
	container := p.Container

	// 10-bit / HDR is a hard no for browser playback even if h264 — needs SW transcode.
	tenBitOrHDR := p.BitDepth >= 10 || p.HDR != ""

	if !tenBitOrHDR && video == "h264" {
		if audio == "aac" {
			if container == ".mp4" {
				return ActionPassthrough
			}
			return ActionRemux
		}
		// Audio incompatible (AC3/DTS/TrueHD/EAC3) → remux video, transcode audio.
		return ActionRemuxAudio
	}

	// HEVC / AV1 / VP9 / 10-bit / unknown → full re-encode video.
	return ActionTranscodeVideo
}

func lowerExt(filePath string) string {
	dot := strings.LastIndex(filePath, ".")
	if dot < 0 {
		return ""
	}
	return strings.ToLower(filePath[dot:])
}
