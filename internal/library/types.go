package library

import "github.com/Unarr-app/unarr-cli/internal/library/mediainfo"

// LibraryItem represents a single scanned media file.
type LibraryItem struct {
	FilePath string `json:"filePath"`
	FileName string `json:"fileName"`
	FileSize int64  `json:"fileSize"`
	ModTime  string `json:"modTime"` // ISO 8601
	// Fingerprint is a stable content identity (see fingerprint.go). Cached so
	// incremental scans reuse it when size+mtime are unchanged.
	Fingerprint string               `json:"fingerprint,omitempty"`
	Title       string               `json:"title"`
	Year        string               `json:"year,omitempty"`
	Season      int                  `json:"season,omitempty"`
	Episode     int                  `json:"episode,omitempty"`
	Quality     string               `json:"quality,omitempty"` // "1080p" etc (from filename)
	Codec       string               `json:"codec,omitempty"`   // "x265" etc (from filename)
	MediaInfo   *mediainfo.MediaInfo `json:"mediaInfo,omitempty"`
	ScanError   string               `json:"scanError,omitempty"`
	// ScanAborted marks a probe that never reached a verdict about the FILE —
	// the scan context was cancelled (daemon restart/shutdown), ffprobe timed
	// out, or it was killed (OOM). The file itself may be perfectly healthy, so
	// these items must NEVER be synced as damaged/"unreadable": doing so flagged
	// ~1.4k good files across the fleet (incident 2026-07-21). They are dropped
	// from the sync payload instead, leaving the previous verdict untouched.
	ScanAborted bool `json:"scanAborted,omitempty"`
}

// LibraryCache is the on-disk cache of scanned library items.
type LibraryCache struct {
	Version   int           `json:"version"`
	ScannedAt string        `json:"scannedAt"`
	Path      string        `json:"path"`
	Items     []LibraryItem `json:"items"`
}

// Bump whenever the scan logic changes in a way that should re-probe an
// existing library on next scan (incremental reuse keys off mtime+size, so a
// pure logic change is invisible without this). v2: file-integrity detection
// (ffprobe corruption / incomplete-download flag).
const cacheVersion = 2
