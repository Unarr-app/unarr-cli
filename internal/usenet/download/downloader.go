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

	"github.com/torrentclaw/torrentclaw-cli/internal/ui"
	"github.com/torrentclaw/torrentclaw-cli/internal/usenet/nntp"
	"github.com/torrentclaw/torrentclaw-cli/internal/usenet/nzb"
	"github.com/torrentclaw/torrentclaw-cli/internal/usenet/yenc"
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
// Returns the path to the assembled file.
func (d *Downloader) DownloadFile(ctx context.Context, file nzb.File, outputDir string, progressCh chan<- Progress) (string, error) {
	fileName := file.Filename()
	if fileName == "" {
		fileName = fmt.Sprintf("usenet_%d", time.Now().UnixNano())
	}

	destPath := filepath.Join(outputDir, fileName)

	// Ensure output directory exists
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}

	totalBytes := file.TotalBytes()
	totalSegs := len(file.Segments)

	// Sort segments by number
	segments := make([]nzb.Segment, len(file.Segments))
	copy(segments, file.Segments)
	sort.Slice(segments, func(i, j int) bool {
		return segments[i].Number < segments[j].Number
	})

	// Create/open output file
	outFile, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("create file: %w", err)
	}
	defer outFile.Close()

	// Pre-allocate file if we know the size
	if totalBytes > 0 {
		outFile.Truncate(totalBytes)
	}

	// Download segments using worker pool
	var downloaded atomic.Int64
	var segsDone atomic.Int32
	startTime := time.Now()

	// Create work channel
	type segWork struct {
		seg   nzb.Segment
		index int
	}
	workCh := make(chan segWork, len(segments))
	for i, seg := range segments {
		workCh <- segWork{seg: seg, index: i}
	}
	close(workCh)

	// Track file offsets for each segment (sequential assembly)
	offsets := make([]int64, len(segments))
	var offset int64
	for i, seg := range segments {
		offsets[i] = offset
		offset += seg.Bytes
	}

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
			}
		}()
	}

	// Wait for all workers
	wg.Wait()

	// Stop progress reporter before sending final progress
	close(stopProgress)

	// Check for errors
	select {
	case err := <-errCh:
		os.Remove(destPath)
		return "", err
	default:
	}

	// Check context cancellation
	if ctx.Err() != nil {
		os.Remove(destPath)
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

	log.Printf("[usenet] downloaded %s (%d segments, %s)", fileName, totalSegs, ui.FormatBytes(actualSize))
	return destPath, nil
}

// DownloadNZB downloads content files from an NZB (rars or direct content).
// Par2 files are NOT downloaded initially — they're only fetched on demand
// if extraction fails (via DownloadPar2).
// Returns a map of filename → filepath for all downloaded files.
func (d *Downloader) DownloadNZB(ctx context.Context, n *nzb.NZB, outputDir string, progressCh chan<- Progress) (map[string]string, error) {
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

	results := make(map[string]string)

	for _, file := range filesToDownload {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		path, err := d.DownloadFile(ctx, file, outputDir, progressCh)
		if err != nil {
			return results, fmt.Errorf("download %s: %w", file.Filename(), err)
		}
		results[file.Filename()] = path
	}

	return results, nil
}

// DownloadPar2 downloads par2 parity files from the NZB.
// Called on-demand when extraction/verification fails.
func (d *Downloader) DownloadPar2(ctx context.Context, n *nzb.NZB, outputDir string, progressCh chan<- Progress) (map[string]string, error) {
	par2Files := n.Par2Files()
	if len(par2Files) == 0 {
		return nil, fmt.Errorf("no par2 files in NZB")
	}

	results := make(map[string]string)
	for _, file := range par2Files {
		path, err := d.DownloadFile(ctx, file, outputDir, progressCh)
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

