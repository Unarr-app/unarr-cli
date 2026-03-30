package download

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/torrentclaw/unarr/internal/usenet/nntp"
	"github.com/torrentclaw/unarr/internal/usenet/nzb"
	"github.com/torrentclaw/unarr/internal/usenet/yenc"
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

// Downloader orchestrates downloading all segments of NZB files via NNTP.
type Downloader struct {
	nntp *nntp.Client
}

// NewDownloader creates a usenet segment downloader.
func NewDownloader(nntpClient *nntp.Client) *Downloader {
	return &Downloader{nntp: nntpClient}
}

// DownloadFile downloads all segments of a single NZB file and assembles them.
// If tracker is non-nil, it is used for resume support: completed segments are
// skipped, and progress is persisted to disk on pause/error.
// fileIndex is the index of this file within the NZB (for the tracker).
// Returns the path to the assembled file.
func (d *Downloader) DownloadFile(ctx context.Context, file nzb.File, fileIndex int, outputDir string, tracker *ProgressTracker, progressCh chan<- Progress) (string, error) {
	fileName := file.Filename()
	if fileName == "" {
		fileName = fmt.Sprintf("usenet_%d", time.Now().UnixNano())
	}

	destPath := filepath.Join(outputDir, fileName)

	// Ensure output directory exists
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}

	// If tracker says this file is fully done, skip entirely
	if tracker != nil && tracker.IsFileDone(fileIndex) {
		if _, err := os.Stat(destPath); err == nil {
			log.Printf("[usenet] skipping %s (fully downloaded in previous run)", fileName)
			return destPath, nil
		}
		// File was marked done but doesn't exist on disk — re-download
	}

	totalBytes := file.TotalBytes()
	totalSegs := len(file.Segments)

	// Sort segments by number
	segments := make([]nzb.Segment, len(file.Segments))
	copy(segments, file.Segments)
	sort.Slice(segments, func(i, j int) bool {
		return segments[i].Number < segments[j].Number
	})

	// Track file offsets for each segment (sequential assembly)
	offsets := make([]int64, len(segments))
	var offset int64
	for i, seg := range segments {
		offsets[i] = offset
		offset += seg.Bytes
	}

	// Open output file — resume-aware
	var outFile *os.File
	var err error
	resuming := false

	if tracker != nil {
		if _, statErr := os.Stat(destPath); statErr == nil && tracker.CompletedSegments(fileIndex) > 0 {
			// Partial file exists and we have progress — open for read-write (no truncate)
			outFile, err = os.OpenFile(destPath, os.O_RDWR, 0o644)
			if err != nil {
				return "", fmt.Errorf("open file for resume: %w", err)
			}
			resuming = true
		}
	}

	if outFile == nil {
		// Fresh start
		outFile, err = os.Create(destPath)
		if err != nil {
			return "", fmt.Errorf("create file: %w", err)
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
		// All segments already done
		log.Printf("[usenet] %s already complete (%d segments)", fileName, totalSegs)
		return destPath, nil
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

				data, err := d.downloadSegment(ctx, work.seg)
				if err != nil {
					select {
					case errCh <- fmt.Errorf("segment %d (%s): %w", work.seg.Number, work.seg.MessageID, err):
					default:
					}
					return
				}

				// Write decoded data at the correct offset
				// WriteAt is safe for concurrent non-overlapping writes
				_, writeErr := outFile.WriteAt(data, offsets[work.index])

				if writeErr != nil {
					select {
					case errCh <- fmt.Errorf("write segment %d: %w", work.seg.Number, writeErr):
					default:
					}
					return
				}

				downloaded.Add(int64(len(data)))
				segsDone.Add(1)

				// Mark segment as completed in tracker
				if tracker != nil {
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
		return "", err
	default:
	}

	// Check context cancellation — keep partial file for resume (don't delete)
	if ctx.Err() != nil {
		if tracker != nil {
			tracker.Flush()
		}
		return "", ctx.Err()
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
			FileName:        fileName,
			SegmentsDone:    totalSegs,
			SegmentsTotal:   totalSegs,
			BytesDownloaded: dl,
			BytesTotal:      totalBytes,
			SpeedBps:        speed,
		}:
		default:
		}
	}

	// Truncate to actual size (in case pre-allocation was larger)
	actualSize := downloaded.Load()
	if actualSize > 0 {
		outFile.Truncate(actualSize)
	}

	log.Printf("[usenet] downloaded %s (%d segments, %s)", fileName, totalSegs, formatBytes(actualSize))
	return destPath, nil
}

// DownloadNZB downloads content files from an NZB (rars or direct content).
// Par2 files are NOT downloaded initially — they're only fetched on demand
// if extraction fails (via DownloadPar2).
// If tracker is non-nil, completed files are skipped and progress is tracked per-segment.
// Returns a map of filename → filepath for all downloaded files.
func (d *Downloader) DownloadNZB(ctx context.Context, n *nzb.NZB, outputDir string, tracker *ProgressTracker, progressCh chan<- Progress) (map[string]string, error) {
	// Determine which files to download (NO par2 initially)
	var filesToDownload []nzb.File

	if n.HasRars() {
		filesToDownload = n.RarFiles()
	} else {
		filesToDownload = n.ContentFiles()
	}

	if len(filesToDownload) == 0 {
		return nil, fmt.Errorf("no downloadable files found in NZB")
	}

	// Build NZB file index mapping: Subject → index in n.Files
	// This maps each file to its position in the ProgressTracker
	nzbFileIndex := make(map[string]int)
	for i, f := range n.Files {
		nzbFileIndex[f.Subject] = i
	}

	results := make(map[string]string)

	for _, file := range filesToDownload {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
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

		path, err := d.DownloadFile(ctx, file, fileIdx, outputDir, tracker, progressCh)
		if err != nil {
			return results, fmt.Errorf("download %s: %w", file.Filename(), err)
		}
		results[file.Filename()] = path
	}

	return results, nil
}

// DownloadPar2 downloads par2 parity files from the NZB.
// Called on-demand when extraction/verification fails.
// No resume tracking — par2 files are small and downloaded fresh.
func (d *Downloader) DownloadPar2(ctx context.Context, n *nzb.NZB, outputDir string, progressCh chan<- Progress) (map[string]string, error) {
	par2Files := n.Par2Files()
	if len(par2Files) == 0 {
		return nil, fmt.Errorf("no par2 files in NZB")
	}

	results := make(map[string]string)
	for _, file := range par2Files {
		path, err := d.DownloadFile(ctx, file, -1, outputDir, nil, progressCh)
		if err != nil {
			log.Printf("[usenet] par2 download failed (non-fatal): %v", err)
			continue
		}
		results[file.Filename()] = path
	}
	return results, nil
}

// downloadSegment downloads and decodes a single segment.
func (d *Downloader) downloadSegment(ctx context.Context, seg nzb.Segment) ([]byte, error) {
	// Download article body via NNTP
	body, err := d.nntp.Body(ctx, seg.MessageID)
	if err != nil {
		return nil, fmt.Errorf("nntp body: %w", err)
	}

	// Decode yEnc
	part, err := yenc.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("yenc decode: %w", err)
	}

	return part.Data, nil
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
