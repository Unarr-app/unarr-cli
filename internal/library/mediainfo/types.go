package mediainfo

// MediaInfo holds the media analysis result from ffprobe.
type MediaInfo struct {
	Video     *VideoInfo      `json:"video"`
	Audio     []AudioTrack    `json:"audio"`
	Subtitles []SubtitleTrack `json:"subtitles"`
	Languages []string        `json:"languages"` // derived from audio tracks
	// Fonts are the font files muxed into the container as attachments. Fansub
	// .ass tracks name fonts the viewer's machine almost never has, so a faithful
	// render needs these shipped alongside the subtitle. Empty for the vast
	// majority of files — only anime/fansub releases carry them.
	Fonts []FontAttachment `json:"fonts,omitempty"`
	// Integrity is non-nil only when the scan found signs of corruption / an
	// incomplete download. Surfaced in the web library as a "damaged" warning
	// so the user re-downloads instead of hitting a file that won't play.
	Integrity *IntegrityInfo `json:"integrity,omitempty"`
}

// IntegrityInfo flags a file whose metadata probed OK enough to land in the
// library but that shows structural damage — the hallmark of an incomplete or
// corrupt download. Reason is a stable code the web localizes; two families:
//
//	header probe (assessIntegrity): "invalid_data", "ebml_corrupt",
//	  "moov_missing", "bitstream_corrupt", "no_duration".
//	deep probe (AssessTruncation): "truncated" (tail data stops before the
//	  header's claimed duration), "tail_corrupt" (bytes short but tail decode
//	  fails).
type IntegrityInfo struct {
	Damaged bool   `json:"damaged"`
	Reason  string `json:"reason,omitempty"`
	// Unverified marks a file the deep probe never managed to check (its tail
	// demux ran past truncProbeTimeout on slow/contended storage) even after the
	// deferred serial retry. It is NOT a verdict: such a file is neither known
	// healthy nor damaged, and Damaged stays false, so it can never be synced as
	// corrupt. It exists so "checked and clean" stops being indistinguishable
	// from "we never got to look" — before this, both were a plain nil verdict
	// and nothing upstream could tell a user what was still unverified.
	Unverified bool `json:"unverified,omitempty"`
}

// VideoInfo represents the primary video stream metadata.
type VideoInfo struct {
	Codec     string  `json:"codec"` // "hevc", "h264", "av1"
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	BitDepth  int     `json:"bitDepth"`  // 8, 10, 12
	HDR       string  `json:"hdr"`       // "HDR10", "DV", "HLG", "DV+HDR10", ""
	FrameRate float64 `json:"frameRate"` // e.g. 23.976
	Profile   string  `json:"profile"`   // e.g. "Main 10", "High"
	Duration  float64 `json:"duration"`  // seconds
}

// AudioTrack represents a single audio stream.
type AudioTrack struct {
	Lang     string `json:"lang"`     // ISO 639-1
	Codec    string `json:"codec"`    // "aac", "ac3", "dts", "truehd"
	Channels int    `json:"channels"` // 2, 6, 8
	Title    string `json:"title"`
	Default  bool   `json:"default"`
}

// SubtitleTrack represents a single subtitle source — either an EMBEDDED stream
// (the common case, identified by its ffmpeg `0:s:N` order in the slice) or an
// EXTERNAL sidecar file sitting next to the media (Path set, External true).
//
// External sidecars (a `.srt`/`.ass`/`.vtt` named after the video, or one in a
// `Subs/` subfolder) are appended AFTER all embedded tracks so the embedded
// tracks keep slice positions equal to their `0:s:N` index — the web's
// resolveSubtitleTracks relies on that for embedded, and switches to Path-based
// addressing for external (served via /sub?p=<file>&i=-1).
type SubtitleTrack struct {
	Lang   string `json:"lang"`
	Codec  string `json:"codec"`
	Title  string `json:"title"`
	Forced bool   `json:"forced"`
	// External is true for a sidecar file; false (omitted) for an embedded stream.
	External bool `json:"external,omitempty"`
	// Path is the absolute filesystem path of the sidecar file (External only).
	// Empty for embedded streams (those live inside the media container).
	Path string `json:"path,omitempty"`
}

// FontAttachment is a font file muxed into the container, needed to render an
// .ass subtitle the way its author typeset it.
type FontAttachment struct {
	// Index addresses the attachment for `ffmpeg -dump_attachment:t:<Index>`.
	//
	// It is the position among ATTACHMENT streams — NOT the global stream index,
	// and NOT the position after filtering to fonts. On a real release those
	// differ wildly: Skeleton Knight S01E01 has attachments t:0..t:25 whose
	// stream indices run 16..41. Getting this wrong silently dumps the wrong
	// file, so the counter must advance for EVERY attachment, font or not.
	Index    int    `json:"index"`
	Filename string `json:"filename"`
	Mimetype string `json:"mimetype,omitempty"`
}
