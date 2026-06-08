package mediainfo

// MediaInfo holds the media analysis result from ffprobe.
type MediaInfo struct {
	Video     *VideoInfo      `json:"video"`
	Audio     []AudioTrack    `json:"audio"`
	Subtitles []SubtitleTrack `json:"subtitles"`
	Languages []string        `json:"languages"` // derived from audio tracks
	// Integrity is non-nil only when the scan found signs of corruption / an
	// incomplete download. Surfaced in the web library as a "damaged" warning
	// so the user re-downloads instead of hitting a file that won't play.
	Integrity *IntegrityInfo `json:"integrity,omitempty"`
}

// IntegrityInfo flags a file whose metadata probed OK enough to land in the
// library but that shows structural damage (ffprobe emitted EBML / "invalid
// data" errors, a truncated moov atom, or no usable video/duration) — the
// hallmark of an incomplete or corrupt download.
type IntegrityInfo struct {
	Damaged bool   `json:"damaged"`
	Reason  string `json:"reason,omitempty"`
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
