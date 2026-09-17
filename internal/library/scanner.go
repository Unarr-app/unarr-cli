package library

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
	"github.com/Unarr-app/unarr-cli/internal/parser"
)

// videoExts is the SINGLE source of truth for what counts as a video file across
// the whole binary. It is a SUPERSET of every extension any subsystem recognises:
// the scanner, the organizer (engine.isVideoFile delegates here), and reconcile's
// hygiene sweep all key off this one map.
//
// Keeping them unified is load-bearing for data safety: a divergence once meant
// reconcile did not recognise a `.m2ts` Blu-ray remux (30 GB) as a video, judged
// its directory "video-less", and RemoveAll'd a legitimate film. Any new video
// container goes HERE and nowhere else. engine has a parity test
// (TestVideoExtParity) that fails if its list drifts from this set.
var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m4v": true,
	".ts": true, ".m2ts": true, ".wmv": true, ".mov": true,
	".webm": true, ".flv": true, ".mpg": true, ".mpeg": true,
	".vob": true,
}

// IsVideoExt reports whether name has a recognised video extension (case-insensitive).
// Exported so other packages (engine) share the ONE canonical set instead of
// maintaining a divergent copy — see videoExts.
func IsVideoExt(name string) bool {
	return videoExts[strings.ToLower(filepath.Ext(name))]
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

	// An already-cancelled scan must report cancellation before ANY setup work:
	// resolving ffprobe can trigger a network auto-download, and on a host without
	// ffprobe its failure replaced context.Canceled in the returned error — the
	// exact signal runAutoScan reads to avoid claiming a fullCycle.
	if err := ctx.Err(); err != nil {
		return nil, err
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

	retryPendingTails(ctx, ffprobePath, ffmpegPath, items)

	return &LibraryCache{
		Version:   cacheVersion,
		ScannedAt: time.Now().UTC().Format(time.RFC3339),
		Path:      dirPath,
		Items:     items,
	}, nil
}

// assessTruncation indirects the deep check so tests can drive the retry's
// branches (expired vs verdict vs clean) without a real ffprobe. Production
// always uses mediainfo.AssessTruncation.
var assessTruncation = mediainfo.AssessTruncation

// retryPendingTails re-runs the deep truncation check on the files whose tail
// probe timed out during the concurrent pass, ONE AT A TIME.
//
// Why serial, and why it works: the timeouts are caused by I/O contention, not
// by file size. Measured on a real NFS library (2026-07-21), the two worst
// files took 37s and 107s with Workers=8 — and 0.2s and 0.8s when re-probed
// alone right afterwards, one of them a 20.8GB 4K remux. Running the leftovers
// with no competing readers therefore converts almost all of them into a real
// verdict for a negligible cost, because by construction only the few files
// that already timed out reach this stage.
//
// The retry can only ever ADD information: a file that still can't be checked
// is marked Unverified (Damaged stays false), never damaged. It walks the
// items slice in place, so callers keep the same LibraryItem values.
func retryPendingTails(ctx context.Context, ffprobePath, ffmpegPath string, items []LibraryItem) {
	pending := 0
	for i := range items {
		if items[i].TailProbePending {
			pending++
		}
	}
	if pending == 0 {
		return
	}
	log.Printf("[scan] %d file(s) had their integrity check time out - retrying serially", pending)

	recovered, unverified := 0, 0
	for i := range items {
		it := &items[i]
		if !it.TailProbePending {
			continue
		}
		// Shutdown mid-retry: leave the rest pending rather than mislabelling
		// them. They are simply files we didn't get to, exactly as before.
		if ctx.Err() != nil {
			break
		}
		if it.MediaInfo == nil || it.MediaInfo.Video == nil {
			it.TailProbePending = false
			continue
		}

		integ, err := assessTruncation(ctx, ffprobePath, ffmpegPath, it.FilePath, it.MediaInfo.Video.Duration)
		switch {
		case integ != nil:
			it.MediaInfo.Integrity = integ
			it.TailProbePending = false
			recovered++
		case errors.Is(err, mediainfo.ErrProbeExpired):
			// Slow even without contention. Record that it went unchecked instead
			// of silently passing it off as healthy — but NEVER as damaged.
			it.MediaInfo.Integrity = &mediainfo.IntegrityInfo{Unverified: true, Reason: "probe_timeout"}
			it.TailProbePending = false
			unverified++
		default:
			// Ran to completion and found nothing wrong: a genuinely clean file.
			it.TailProbePending = false
			recovered++
		}
	}
	log.Printf("[scan] integrity retry done: %d checked, %d still unverified", recovered, unverified)
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
	//
	// An Unverified item also carries a non-nil Integrity, so it falls through
	// here too — deliberately: a file we never managed to check gets another
	// chance next cycle, when the storage may be idle. It can't loop forever
	// because each pass recomputes the verdict from scratch, so the first
	// uncontended run resolves it.
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
		integ, err := mediainfo.AssessTruncation(ctx, ffprobePath, ffmpegPath, filePath, mi.Video.Duration)
		if integ != nil {
			mi.Integrity = integ
		} else if errors.Is(err, mediainfo.ErrProbeExpired) {
			// The deep check ran out of time on slow storage. The file itself is
			// fine as far as we know — keep every bit of metadata we extracted and
			// just remember that the truncation check still owes us an answer.
			item.TailProbePending = true
		}
	}
	item.MediaInfo = mi

	return item
}

// isZeroContentStub reports whether a video file of acceptable APPARENT size
// holds no playable bytes, and why. Non-destructive: the caller skips the file,
// nothing is ever deleted (that is `library clean --apply`'s job, gated behind
// Cleanup.RemoveCorruptVideos).
//
// This closes the hole that let an EMPTY file become a playable-looking library
// item. A torrent client preallocates every file in a pack at its final size, so
// a release that never downloaded a byte is a 553 MB apparent / 0 block sparse
// file. discoverFiles gates on info.Size(), which reports the apparent size, so
// the stub sailed past the 100 MB floor, got scanned, and was published as a
// library_item. The web then offered it in the player and handed VLC a URL, and
// both failed with "Invalid data found when processing input" — a dead-end with
// nothing pointing at the cause (La Brea S01E01, 2026-09-17).
//
// Two signals, both certain rather than heuristic:
//
//   - NO REAL BLOCKS. diskUsage(info) counts st_blocks*512, what `du` reports and
//     what actually frees on delete. Zero real blocks behind a non-zero apparent
//     size means the whole file is holes. A POSIX-only signal: diskUsage falls
//     back to the apparent size on Windows, where this check is a silent no-op —
//     hence the second signal.
//   - ZEROED HEADER. The first 4 KiB is all NUL, so there is no container magic
//     and no demuxer can open the file. Catches the non-sparse variant (a
//     preallocation the filesystem materialised as real zero blocks, common on
//     the NFS/SMB NAS mounts agents use) and covers Windows.
//
// Order matters for COST, not just for correctness. The block check is free — it
// reads the stat discoverFiles already performed — so it is tried first, and the
// only files that pay for a read are those that survive it. The read itself is
// one page (see headerProbe): this runs in discoverFiles, upstream of the
// incremental cache, so it is charged to EVERY file on EVERY cycle, unlike
// ComputeFingerprint's 1 MiB head, which unchanged files skip.
//
// Deliberately NOT a completeness check. A download at 86 % with a real header
// still plays from the start and stays indexed exactly as before; there is no
// percentage threshold here to tune.
func isZeroContentStub(path string, info os.FileInfo) (bool, string) {
	if diskUsage(info) == 0 {
		return true, "sparse stub, 0 bytes on disk (download never started)"
	}

	zero, err := HeaderAllZero(path, info.Size())
	if err != nil {
		// Unreadable is not the same as empty — an EACCES or a torn NFS handle
		// must not silently drop a real file out of the user's library.
		return false, ""
	}
	if zero {
		return true, "header is all zeros (no container magic)"
	}
	return false, ""
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

		// A file of the right APPARENT size that holds no playable bytes is not
		// a library item. See isZeroContentStub — this gate exists because the
		// size floor above reads info.Size(), which for a sparse preallocated
		// download reports the full release size while the file is a hole.
		if stub, why := isZeroContentStub(path, info); stub {
			log.Printf("[scan] skipping %s: %s", filepath.Base(path), why)
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
