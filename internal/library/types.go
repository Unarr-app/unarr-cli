package library

import "github.com/torrentclaw/unarr/internal/library/mediainfo"

// LibraryItem represents a single scanned media file.
type LibraryItem struct {
	FilePath  string              `json:"filePath"`
	FileName  string              `json:"fileName"`
	FileSize  int64               `json:"fileSize"`
	ModTime   string              `json:"modTime"` // ISO 8601
	Title     string              `json:"title"`
	Year      string              `json:"year,omitempty"`
	Season    int                 `json:"season,omitempty"`
	Episode   int                 `json:"episode,omitempty"`
	Quality   string              `json:"quality,omitempty"` // "1080p" etc (from filename)
	Codec     string              `json:"codec,omitempty"`   // "x265" etc (from filename)
	MediaInfo *mediainfo.MediaInfo `json:"mediaInfo,omitempty"`
	ScanError string              `json:"scanError,omitempty"`
}

// LibraryCache is the on-disk cache of scanned library items.
type LibraryCache struct {
	Version   int           `json:"version"`
	ScannedAt string        `json:"scannedAt"`
	Path      string        `json:"path"`
	Items     []LibraryItem `json:"items"`
}

const cacheVersion = 1
