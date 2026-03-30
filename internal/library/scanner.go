package library

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/torrentclaw/unarr/internal/library/mediainfo"
	"github.com/torrentclaw/unarr/internal/parser"
)

// videoExts are file extensions considered as video files.
var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m4v": true,
	".ts": true, ".wmv": true, ".mov": true, ".webm": true,
	".flv": true, ".mpg": true, ".mpeg": true, ".vob": true,
}

// excludePatterns are path substrings that indicate non-content files.
var excludePatterns = []string{
	"sample", "trailer", "featurette", "extras", "bonus",
	"behind the scenes", "deleted scenes", "interview",
}

const minFileSize = 100 * 1024 * 1024 // 100MB minimum

// ScanOptions configures the library scanner.
type ScanOptions struct {
	Workers     int    // concurrent ffprobe processes (default 8)
	FFprobePath string // explicit path, or auto-resolve
	Incremental bool   // skip unchanged files (mtime+size match cache)
	OnProgress  func(scanned, total int, current string)
}

// Scan walks a directory recursively, finds video files, and runs ffprobe on each.
func Scan(ctx context.Context, dirPath string, existing *LibraryCache, opts ScanOptions) (*LibraryCache, error) {
	if opts.Workers <= 0 {
		opts.Workers = 8
	}

	// Resolve ffprobe
	ffprobePath, err := mediainfo.ResolveFFprobe(opts.FFprobePath)
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}

	// Discover video files
	files, err := discoverFiles(dirPath)
	if err != nil {
		return nil, fmt.Errorf("discover files: %w", err)
	}

	if len(files) == 0 {
		return &LibraryCache{
			Version:   cacheVersion,
			ScannedAt: time.Now().UTC().Format(time.RFC3339),
			Path:      dirPath,
		}, nil
	}

	// Build cache index for incremental mode
	cacheIdx := BuildCacheIndex(existing)

	// Scan files concurrently
	var (
		scanned atomic.Int32
		total   = len(files)
		mu      sync.Mutex
		items   = make([]LibraryItem, 0, total)
	)

	sem := make(chan struct{}, opts.Workers)
	var wg sync.WaitGroup

	for _, filePath := range files {
		select {
		case <-ctx.Done():
			break
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(fp string) {
			defer wg.Done()
			defer func() { <-sem }()

			item := scanSingleFile(ctx, ffprobePath, fp, cacheIdx, existing, opts.Incremental)

			mu.Lock()
			items = append(items, item)
			mu.Unlock()

			n := int(scanned.Add(1))
			if opts.OnProgress != nil {
				opts.OnProgress(n, total, filepath.Base(fp))
			}
		}(filePath)
	}

	wg.Wait()

	return &LibraryCache{
		Version:   cacheVersion,
		ScannedAt: time.Now().UTC().Format(time.RFC3339),
		Path:      dirPath,
		Items:     items,
	}, nil
}

func scanSingleFile(ctx context.Context, ffprobePath, filePath string, cacheIdx map[string]int, existing *LibraryCache, incremental bool) LibraryItem {
	info, err := os.Stat(filePath)
	if err != nil {
		return LibraryItem{
			FilePath:  filePath,
			FileName:  filepath.Base(filePath),
			ScanError: err.Error(),
		}
	}

	item := LibraryItem{
		FilePath: filePath,
		FileName: filepath.Base(filePath),
		FileSize: info.Size(),
		ModTime:  info.ModTime().UTC().Format(time.RFC3339),
	}

	// Parse filename for title, year, quality, codec
	parsed := parser.Parse(item.FileName)
	item.Quality = parsed.Quality
	item.Codec = parsed.Codec
	item.Year = parsed.Year

	// Extract title from filename
	item.Title = CleanTitle(item.FileName)
	if item.Title == "" {
		item.Title = item.FileName
	}

	// Parse season/episode
	item.Season, item.Episode = ParseSeasonEpisode(item.FileName)

	// Incremental: skip if file hasn't changed
	if incremental && existing != nil {
		if idx, ok := cacheIdx[filePath]; ok {
			cached := existing.Items[idx]
			if cached.FileSize == item.FileSize && cached.ModTime == item.ModTime && cached.MediaInfo != nil {
				item.MediaInfo = cached.MediaInfo
				return item
			}
		}
	}

	// Run ffprobe
	mi, err := mediainfo.ExtractMediaInfo(ctx, ffprobePath, filePath)
	if err != nil {
		item.ScanError = err.Error()
		return item
	}
	item.MediaInfo = mi

	return item
}

// discoverFiles walks a directory and returns paths of video files.
func discoverFiles(root string) ([]string, error) {
	var files []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip errors, continue walking
		}

		if d.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if !videoExts[ext] {
			return nil
		}

		// Check file size (stat is lazy on some systems)
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() < minFileSize {
			return nil
		}

		// Exclude non-content files
		lower := strings.ToLower(path)
		for _, pattern := range excludePatterns {
			if strings.Contains(lower, pattern) {
				return nil
			}
		}

		files = append(files, path)
		return nil
	})

	return files, err
}
