package download

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// Progress is emitted during download.
type Progress struct {
	FileName        string
	SegmentsDone    int
	SegmentsTotal   int
	BytesDownloaded int64
	BytesTotal      int64
	SpeedBps        int64
}

// MissingSegment records a segment that could not be retrieved or decoded.
// Articles expire off Usenet retention unevenly, so a handful of holes in an
// otherwise intact download is the NORMAL case that par2 recovery exists to
// repair — see Downloader.MissingTolerance.
type MissingSegment struct {
	File      string
	Number    int
	MessageID string
	Err       error
}

// Downloader orchestrates downloading all segments of NZB files via NNTP.
type Downloader struct {
	nntp *nntp.Client

	// MissingTolerance is the fraction of a file's segments that may fail
	// before the download is abandoned. Zero (the default) aborts on the first
	// failure — correct when there is no parity to repair the hole with.
	//
	// The engine raises it when the NZB ships par2: aborting the whole file on
	// one expired article defeats the entire point of parity, which is
	// precisely to reconstruct missing blocks. Holes are left as the
	// pre-allocated zeros and handed to par2, which repairs them from the
	// recovery volumes.
	MissingTolerance float64
}

// NewDownloader creates a usenet segment downloader.
func NewDownloader(nntpClient *nntp.Client) *Downloader {
	return &Downloader{nntp: nntpClient}
}

// validatePartOffset rejects a yEnc part whose declared position can't be
// trusted as a write offset. Articles come from third-party servers and
// yenc.Decode does not sanity-check =ypart at all (a hostile or broken post can
// carry a valid CRC32 over a nonsense begin=), so this is the only guard
// between the wire and both an unbounded WriteAt and a persisted file size.
//
// totalBytes is the NZB's encoded byte sum, always >= the decoded size, so it
// is a safe upper bound.
func validatePartOffset(part *yenc.Part, totalBytes int64) error {
	if part.Begin <= 0 {
		return fmt.Errorf("yenc: unusable =ypart begin=%d", part.Begin)
	}
	end := part.Begin - 1 + int64(len(part.Data))
	if totalBytes > 0 && end > totalBytes {
		return fmt.Errorf("yenc: =ypart begin=%d + %d bytes ends at %d, past the file's %d",
			part.Begin, len(part.Data), end, totalBytes)
	}
	return nil
}

// tailSegmentMissing reports whether the highest-numbered segment of the file
// is one of the ones that could not be fetched — i.e. whether the end of the
// file is a hole rather than real data.
func tailSegmentMissing(segments []nzb.Segment, missing []MissingSegment) bool {
	if len(missing) == 0 || len(segments) == 0 {
		return false
	}
	last := segments[len(segments)-1]
	for _, m := range missing {
		if m.MessageID == last.MessageID {
			return true
		}
	}
	return false
}

// maxMissing returns how many segment failures are survivable for a file of
// segCount segments. Always at least 1 once a tolerance is set, so a tiny file
// still benefits from parity.
func (d *Downloader) maxMissing(segCount int) int {
	if d.MissingTolerance <= 0 {
		return 0
	}
	n := int(float64(segCount) * d.MissingTolerance)
	if n < 1 {
		n = 1
	}
	return n
}

// DownloadFile downloads all segments of a single NZB file and assembles them.
// If tracker is non-nil, it is used for resume support: completed segments are
// skipped, and progress is persisted to disk on pause/error.
// fileIndex is the index of this file within the NZB (for the tracker).
// Returns the path to the assembled file plus any segments that could not be
// fetched (empty unless MissingTolerance allows the download to proceed with
// holes for par2 to repair).
func (d *Downloader) DownloadFile(ctx context.Context, file nzb.File, fileIndex int, outputDir string, tracker *ProgressTracker, progressCh chan<- Progress) (string, []MissingSegment, error) {
	// Confine the filename to outputDir. File.Filename() is a raw substring of
	// the NZB subject (third-party indexer content) and can carry "/" or ".." —
	// filepath.Base strips any path, so a "../../x" subject can't escape the
	// task dir. This is the single sink for both content and par2 writes.
	fileName := filepath.Base(file.Filename())
	if fileName == "" || fileName == "." || fileName == ".." || fileName == string(os.PathSeparator) {
		fileName = fmt.Sprintf("usenet_%d", time.Now().UnixNano())
	}

	destPath := filepath.Join(outputDir, fileName)

	// Ensure output directory exists
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("mkdir: %w", err)
	}

	// If tracker says this file is fully done, skip entirely
	if tracker != nil && tracker.IsFileDone(fileIndex) {
		if _, err := os.Stat(destPath); err == nil {
			log.Printf("[usenet] skipping %s (fully downloaded in previous run)", fileName)
			return destPath, nil, nil
		}
		// Marked done but gone from disk (user deleted it, a failed move, a
		// wiped download dir). The bits MUST be cleared here: leaving them set
		// makes every segment "already done" below, so the run creates an empty
		// pre-allocated file, downloads nothing, and returns a file of pure
		// zeros as a successful download.
		log.Printf("[usenet] %s was marked complete but is missing from disk — re-downloading", fileName)
		tracker.ResetFile(fileIndex)
	}

	totalBytes := file.TotalBytes()
	totalSegs := len(file.Segments)

	// Sort segments into assembly order. The per-segment START OFFSET is NOT
	// taken from the NZB: Segment.Bytes is the ENCODED article size (yEnc
	// escaping + line breaks + the =ybegin/=ypart/=yend headers, ~3% above the
	// decoded payload), so accumulating it yields offsets that drift further
	// from the truth with every segment. Writing decoded bytes at those
	// positions shreds the file — gaps between segments and a ~3% overrun at
	// the end that the old final Truncate then chopped off. par2 saw hundreds
	// of damaged blocks and declared the delivery unrepairable.
	//
	// The authoritative offset is the yEnc "=ypart begin=" header carried by
	// the article itself (1-based), read per segment below.
	segments := nzb.SortSegmentsByNumber(file.Segments)

	// Open output file — resume-aware
	var outFile *os.File
	var err error
	resuming := false

	if tracker != nil {
		if _, statErr := os.Stat(destPath); statErr == nil && tracker.CompletedSegments(fileIndex) > 0 {
			// Partial file exists and we have progress — open for read-write (no truncate)
			outFile, err = os.OpenFile(destPath, os.O_RDWR, 0o644)
			if err != nil {
				return "", nil, fmt.Errorf("open file for resume: %w", err)
			}
			resuming = true
		}
	}

	if outFile == nil {
		// Fresh start
		outFile, err = os.Create(destPath)
		if err != nil {
			return "", nil, fmt.Errorf("create file: %w", err)
		}
		// Pre-allocate file if we know the size
		if totalBytes > 0 {
			outFile.Truncate(totalBytes)
		}
	}
	defer outFile.Close()

	// Download segments using worker pool
	var downloaded atomic.Int64
	var segsDone atomic.Int32
	// maxEnd is the highest end-offset written, i.e. the file's real decoded
	// size once every segment has landed. Seeded from the tracker so a resumed
	// run inherits what earlier runs established — otherwise a resume that
	// fetches only middle segments would truncate the file below its true tail.
	var maxEnd atomic.Int64
	if tracker != nil {
		maxEnd.Store(tracker.KnownSize(fileIndex))
	}
	var missingMu sync.Mutex
	var missing []MissingSegment
	missingBudget := d.maxMissing(totalSegs)
	startTime := time.Now()

	// Create work channel — skip already-completed segments
	type segWork struct {
		seg   nzb.Segment
		index int
	}

	pendingCount := 0
	for i := range segments {
		if tracker != nil && tracker.IsDone(fileIndex, i) {
			// Already downloaded — count towards progress
			downloaded.Add(segments[i].Bytes)
			segsDone.Add(1)
		} else {
			pendingCount++
		}
	}

	if resuming {
		log.Printf("[usenet] resuming %s (%d/%d segments, %s/%s)",
			fileName, totalSegs-pendingCount, totalSegs,
			formatBytes(downloaded.Load()), formatBytes(totalBytes))
	}

	if pendingCount == 0 {
		// All segments already done. Still size the file: a previous run may have
		// died between its last MarkDone and its closing Truncate, leaving the
		// pre-allocation slack (encoded-size zeros) on the tail — which par2
		// reads as damage.
		if tracker != nil {
			if known := tracker.KnownSize(fileIndex); known > 0 {
				if err := outFile.Truncate(known); err != nil {
					return "", nil, fmt.Errorf("truncate %s to %d: %w", fileName, known, err)
				}
			}
		}
		log.Printf("[usenet] %s already complete (%d segments)", fileName, totalSegs)
		return destPath, nil, nil
	}

	workCh := make(chan segWork, pendingCount)
	for i, seg := range segments {
		if tracker == nil || !tracker.IsDone(fileIndex, i) {
			workCh <- segWork{seg: seg, index: i}
		}
	}
	close(workCh)

	// Progress reporter goroutine
	stopProgress := make(chan struct{})
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				dl := downloaded.Load()
				elapsed := time.Since(startTime).Seconds()
				var speed int64
				if elapsed > 0 {
					speed = int64(float64(dl) / elapsed)
				}
				if progressCh != nil {
					select {
					case progressCh <- Progress{
						FileName:        fileName,
						SegmentsDone:    int(segsDone.Load()),
						SegmentsTotal:   totalSegs,
						BytesDownloaded: dl,
						BytesTotal:      totalBytes,
						SpeedBps:        speed,
					}:
					default:
					}
				}
			case <-stopProgress:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	// Workers — one per NNTP connection
	numWorkers := d.nntp.ActiveConnections()
	if numWorkers <= 0 {
		numWorkers = 1
	}

	errCh := make(chan error, 1)
	var wg sync.WaitGroup

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for work := range workCh {
				select {
				case <-ctx.Done():
					return
				default:
				}

				part, err := d.downloadSegment(ctx, work.seg)
				if err == nil {
					// The =ypart offset comes from a third-party article and is
					// used verbatim as a WriteAt position, so it must be bounded
					// before it becomes a file size. An absurd begin= would
					// WriteAt far past the end (a real allocation on the NFS
					// mounts prod uses, bypassing the pre-flight disk check) and,
					// worse, get persisted as knownSize — poisoning the truncate
					// of every future retry. A begin of 0 on a multipart body is
					// equally unusable: falling back to the estimated offset is
					// exactly the corruption this rewrite removed.
					err = validatePartOffset(part, totalBytes)
				}
				if err != nil {
					// A hole, not necessarily a dead download. With parity
					// available we record it and keep going so par2 can
					// reconstruct it; the pre-allocated zeros stay in place and
					// the segment is deliberately NOT marked done, so a later
					// retry re-attempts exactly these articles.
					missingMu.Lock()
					missing = append(missing, MissingSegment{
						File: fileName, Number: work.seg.Number, MessageID: work.seg.MessageID, Err: err,
					})
					over := len(missing) > missingBudget
					count := len(missing)
					missingMu.Unlock()

					if over {
						select {
						case errCh <- fmt.Errorf("segment %d (%s): %w (%d/%d segments unavailable, past the %d tolerated)",
							work.seg.Number, work.seg.MessageID, err, count, totalSegs, missingBudget):
						default:
						}
						return
					}
					continue
				}

				// Write decoded data at the offset the article itself declares
				// (=ypart begin= is 1-based; yenc.Decode synthesises begin=1 for
				// a single-part body). Validated above, so it is always usable.
				// WriteAt is safe for concurrent non-overlapping writes.
				off := part.Begin - 1
				_, writeErr := outFile.WriteAt(part.Data, off)

				if writeErr != nil {
					select {
					case errCh <- fmt.Errorf("write segment %d: %w", work.seg.Number, writeErr):
					default:
					}
					return
				}

				end := off + int64(len(part.Data))
				for {
					cur := maxEnd.Load()
					if end <= cur || maxEnd.CompareAndSwap(cur, end) {
						break
					}
				}

				// Count the ENCODED size, matching both BytesTotal (the NZB's
				// encoded sum) and the resumed segments counted above. Adding
				// decoded lengths here mixed two units, so a complete download
				// reported ~97% and stopped there.
				downloaded.Add(work.seg.Bytes)
				segsDone.Add(1)

				// Mark segment as completed in tracker
				if tracker != nil {
					tracker.NoteFileSize(fileIndex, end)
					tracker.MarkDone(fileIndex, work.index)
				}
			}
		}()
	}

	// Wait for all workers
	wg.Wait()

	// Stop progress reporter before sending final progress
	close(stopProgress)

	// Check for errors — keep partial file for resume (don't delete)
	select {
	case err := <-errCh:
		if tracker != nil {
			tracker.Flush()
		}
		return "", missing, err
	default:
	}

	// Check context cancellation — keep partial file for resume (don't delete)
	if ctx.Err() != nil {
		if tracker != nil {
			tracker.Flush()
		}
		return "", missing, ctx.Err()
	}

	// Final progress report
	dl := downloaded.Load()
	elapsed := time.Since(startTime).Seconds()
	var speed int64
	if elapsed > 0 {
		speed = int64(float64(dl) / elapsed)
	}
	if progressCh != nil {
		select {
		case progressCh <- Progress{
			FileName: fileName,
			// Report what actually landed. Claiming totalSegs unconditionally
			// hid tolerated holes behind a "100%" final tick.
			SegmentsDone:    int(segsDone.Load()),
			SegmentsTotal:   totalSegs,
			BytesDownloaded: dl,
			BytesTotal:      totalBytes,
			SpeedBps:        speed,
		}:
		default:
		}
	}

	// Size the file to its real end offset, undoing the pre-allocation slack
	// (which is based on the inflated encoded totals). This MUST be the highest
	// =ypart end seen — never the sum of decoded bytes, which undershoots by
	// exactly the amount any missing segment left as a hole and would chop a
	// repairable tail off the file before par2 ever sees it.
	// Skip it when the file's LAST segment is among the missing: maxEnd then
	// stops at the highest hole-free offset and truncating would cut the tail
	// off — putting the damage beyond par2's reach, which is the opposite of
	// what the tolerance is for. The pre-allocated slack is the lesser evil;
	// par2 repairs a too-long tail, never a missing one.
	actualSize := maxEnd.Load()
	if actualSize > 0 && !tailSegmentMissing(segments, missing) {
		// This truncate is what fixes the encoded-vs-decoded size mismatch — a
		// silent failure here ships a wrong-sized file that par2 then rejects
		// for no visible reason.
		if err := outFile.Truncate(actualSize); err != nil {
			return "", missing, fmt.Errorf("truncate %s to %d: %w", fileName, actualSize, err)
		}
	}

	if len(missing) > 0 {
		log.Printf("[usenet] downloaded %s (%d/%d segments, %s) — %d segments unavailable, leaving holes for par2",
			fileName, totalSegs-len(missing), totalSegs, formatBytes(actualSize), len(missing))
	} else {
		log.Printf("[usenet] downloaded %s (%d segments, %s)", fileName, totalSegs, formatBytes(actualSize))
	}
	return destPath, missing, nil
}

// DownloadNZB downloads content files from an NZB (rars or direct content).
// Par2 files are NOT downloaded here — the engine fetches the small par2 index
// up front and the recovery volumes on demand (via DownloadPar2Files) when
// verification detects damage.
// If tracker is non-nil, completed files are skipped and progress is tracked per-segment.
// Returns a map of filename → filepath for all downloaded files, plus every
// segment that could not be fetched (see Downloader.MissingTolerance) so the
// caller can insist on par2 verification before trusting the result.
func (d *Downloader) DownloadNZB(ctx context.Context, n *nzb.NZB, outputDir string, tracker *ProgressTracker, progressCh chan<- Progress) (map[string]string, []MissingSegment, error) {
	// Determine which files to download (NO par2 initially)
	var filesToDownload []nzb.File

	if n.HasRars() {
		filesToDownload = n.RarFiles()
	} else {
		filesToDownload = n.ContentFiles()
	}

	if len(filesToDownload) == 0 {
		return nil, nil, fmt.Errorf("no downloadable files found in NZB")
	}

	// Build NZB file index mapping: Subject → index in n.Files
	// This maps each file to its position in the ProgressTracker
	nzbFileIndex := make(map[string]int)
	for i, f := range n.Files {
		nzbFileIndex[f.Subject] = i
	}

	results := make(map[string]string)
	var missing []MissingSegment

	for _, file := range filesToDownload {
		select {
		case <-ctx.Done():
			return results, missing, ctx.Err()
		default:
		}

		fileIdx, ok := nzbFileIndex[file.Subject]
		if !ok {
			fileIdx = -1 // unknown index — tracker will treat as no-op
		}

		// Skip fully completed files
		if tracker != nil && tracker.IsFileDone(fileIdx) {
			destPath := filepath.Join(outputDir, file.Filename())
			if _, err := os.Stat(destPath); err == nil {
				results[file.Filename()] = destPath
				log.Printf("[usenet] skipping %s (complete)", file.Filename())
				continue
			}
		}

		path, fileMissing, err := d.DownloadFile(ctx, file, fileIdx, outputDir, tracker, progressCh)
		missing = append(missing, fileMissing...)
		if err != nil {
			return results, missing, fmt.Errorf("download %s: %w", file.Filename(), err)
		}
		results[file.Filename()] = path
	}

	return results, missing, nil
}

// DownloadPar2Files downloads the given par2 parity files. Called lazily: the
// small index up front for verification, the (potentially large) recovery
// volumes only when verification detects damage. No resume tracking — but a
// volume already fully on disk from a prior integrity attempt is skipped
// (os.Create would re-truncate it), so a 3× integrity retry doesn't re-fetch
// the whole parity set each time. Per-file failures are skipped (logged) so one
// missing parity article doesn't abort the set; it errors only when NOTHING
// could be fetched, and the caller decides whether the remainder is enough.
func (d *Downloader) DownloadPar2Files(ctx context.Context, files []nzb.File, outputDir string, progressCh chan<- Progress) (map[string]string, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("no par2 files requested")
	}

	results := make(map[string]string)
	for _, file := range files {
		name := file.Filename()
		// Skip a par2 already fully downloaded (size match). par2 volumes are
		// self-verifying, so a size match is a safe cheap guard; a short one is
		// re-fetched. filepath.Base mirrors DownloadFile's confinement so the
		// stat path matches the eventual write path.
		dest := filepath.Join(outputDir, filepath.Base(name))
		if fi, statErr := os.Stat(dest); statErr == nil && fi.Size() == file.TotalBytes() {
			results[name] = dest
			continue
		}
		// Parity files get NO missing-segment tolerance: a par2 volume with a
		// hole is worse than no volume at all, since par2 would consume it as
		// authoritative recovery data. Any failure drops the file from the set
		// and the caller works with the volumes that did arrive intact.
		par2Dl := &Downloader{nntp: d.nntp}
		path, _, err := par2Dl.DownloadFile(ctx, file, -1, outputDir, nil, progressCh)
		if err != nil {
			log.Printf("[usenet] par2 download failed for %s (non-fatal): %v", name, err)
			continue
		}
		results[name] = path
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("all %d par2 downloads failed", len(files))
	}
	return results, nil
}

// downloadSegment downloads and decodes a single segment. It returns the whole
// yEnc part, not just the payload: Part.Begin carries the article's own
// declaration of where its bytes belong in the assembled file, which is the
// only trustworthy offset (the NZB's byte counts are encoded sizes).
func (d *Downloader) downloadSegment(ctx context.Context, seg nzb.Segment) (*yenc.Part, error) {
	// Download article body via NNTP
	body, err := d.nntp.Body(ctx, seg.MessageID)
	if err != nil {
		return nil, fmt.Errorf("nntp body: %w", err)
	}

	// Decode yEnc (verifies the part's CRC32 when the article carries one)
	part, err := yenc.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("yenc decode: %w", err)
	}

	return part, nil
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
