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

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
	"github.com/Unarr-app/unarr-cli/internal/parser"
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
	FFmpegPath  string // explicit path, or auto-resolve; "" disables the decode-confirm (C)
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

	// Resolve ffmpeg best-effort — it only powers the tail decode-confirm (check
	// C). When absent the truncation checks still run on ffprobe alone (A + B).
	ffmpegPath, ffmpegErr := mediainfo.ResolveFFmpeg(opts.FFmpegPath)
	if ffmpegErr != nil {
		ffmpegPath = ""
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

scanLoop:
	for _, filePath := range files {
		// `break` inside a select only leaves the SELECT, not the loop — the
		// original code kept spawning a probe for every remaining file after
		// cancellation, and each one failed instantly with "context canceled",
		// which BuildSyncItems then synced as damaged/"unreadable". That is how a
		// single daemon restart mid-scan flagged a whole library as broken
		// (incident 2026-07-21). Label the loop and break out of it for real.
		select {
		case <-ctx.Done():
			break scanLoop
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(fp string) {
			defer wg.Done()
			defer func() { <-sem }()

			item := scanSingleFile(ctx, ffprobePath, ffmpegPath, fp, cacheIdx, existing, opts.Incremental)

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

	// A cancelled scan saw only PART of the library, so its item list is not a
	// statement of what exists on disk. Returning it with a nil error let the
	// caller mark the session fullCycle=true, and the server's stale-cleanup
	// then DELETEs every row the truncated scan never reached. Fail loudly so
	// runAutoScan skips the sync (it already clears fullCycle on a scan error).
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("scan of %s interrupted after %d/%d files: %w", dirPath, len(items), total, err)
	}

	return &LibraryCache{
		Version:   cacheVersion,
		ScannedAt: time.Now().UTC().Format(time.RFC3339),
		Path:      dirPath,
		Items:     items,
	}, nil
}

func scanSingleFile(ctx context.Context, ffprobePath, ffmpegPath, filePath string, cacheIdx map[string]int, existing *LibraryCache, incremental bool) LibraryItem {
	info, err := os.Stat(filePath)
	if err != nil {
		// A stat failure says nothing about the file's CONTENT — the mount went
		// away, an NFS/SMB share hiccuped, or permissions are wrong. Treating it
		// as "damaged" told users their files were corrupt every time a network
		// share blipped. Abort the item instead so the existing verdict stands.
		return LibraryItem{
			FilePath:    filePath,
			FileName:    filepath.Base(filePath),
			ScanError:   err.Error(),
			ScanAborted: true,
		}
	}

	item := LibraryItem{
		FilePath: filePath,
		FileName: filepath.Base(filePath),
		FileSize: info.Size(),
		ModTime:  info.ModTime().UTC().Format(time.RFC3339),
	}

	// Look up the cached entry once — reused for both fingerprint reuse and the
	// incremental ffprobe skip below.
	var cached *LibraryItem
	if existing != nil {
		if idx, ok := cacheIdx[filePath]; ok {
			cached = &existing.Items[idx]
		}
	}
	unchanged := cached != nil &&
		cached.FileSize == item.FileSize && cached.ModTime == item.ModTime

	// Fingerprint: reuse the cached value when the file is unchanged and already
	// has one; otherwise compute it (cheap, two bounded reads). Computed even on
	// the incremental path so every synced item carries a stable identity.
	if unchanged && cached.Fingerprint != "" {
		item.Fingerprint = cached.Fingerprint
	} else if fp, fpErr := ComputeFingerprint(filePath, item.FileSize); fpErr == nil {
		item.Fingerprint = fp
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

	// Incremental: skip if file hasn't changed. EXCEPT a previously-damaged
	// file is always re-probed — a re-download to the same path can land with
	// an identical size+mtime (some torrent clients preserve the torrent's
	// mtime), so trusting the cached "damaged" verdict would pin a now-healthy
	// file as broken forever. Re-probing damaged items is cheap (they're few).
	if incremental && unchanged &&
		cached.MediaInfo != nil && cached.MediaInfo.Integrity == nil {
		item.MediaInfo = cached.MediaInfo
		return item
	}

	// Run ffprobe
	mi, err := mediainfo.ExtractMediaInfo(ctx, ffprobePath, filePath)
	if err != nil {
		item.ScanError = err.Error()
		// Only a probe that RAN and rejected the container is evidence the file
		// is broken. A cancelled context, a timeout, an OOM-kill, or a probe that
		// never started says nothing about the file — flagging those as damaged
		// is what marked ~1.4k healthy files across the fleet (2026-07-21).
		item.ScanAborted = mediainfo.IsInconclusiveProbeError(ctx, err)
		return item
	}

	// Deep truncation checks (A/B/C): the header probe above can't see a
	// truncated download whose start-of-file duration is intact. Only when the
	// header itself flagged nothing — a more specific verdict (moov_missing,
	// ebml_corrupt, no_duration) is never overwritten — and we have a real video
	// duration to compare the tail against.
	if mi.Integrity == nil && mi.Video != nil && mi.Video.Duration > 0 {
		if integ := mediainfo.AssessTruncation(ctx, ffprobePath, ffmpegPath, filePath, mi.Video.Duration); integ != nil {
			mi.Integrity = integ
		}
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
