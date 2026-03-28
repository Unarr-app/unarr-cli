package engine

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// httpClient is used for debrid HTTPS downloads with a reasonable header timeout.
var httpClient = &http.Client{
	Transport: &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
	},
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// DebridDownloader downloads files via HTTPS direct URLs resolved by the server.
// The server handles all debrid provider interaction; this downloader only needs
// a plain HTTPS URL to fetch.
type DebridDownloader struct {
	activeMu sync.Mutex
	active   map[string]context.CancelFunc
}

// NewDebridDownloader creates a debrid downloader.
func NewDebridDownloader() *DebridDownloader {
	return &DebridDownloader{
		active: make(map[string]context.CancelFunc),
	}
}

func (d *DebridDownloader) Method() DownloadMethod { return MethodDebrid }

// Available returns true if the task has a direct HTTPS URL from the server.
func (d *DebridDownloader) Available(_ context.Context, task *Task) (bool, error) {
	return task.DirectURL != "", nil
}

// Download fetches the file from task.DirectURL via HTTPS with progress reporting.
// Supports resume via HTTP Range headers if the server supports it.
func (d *DebridDownloader) Download(ctx context.Context, task *Task, outputDir string, progressCh chan<- Progress) (*Result, error) {
	if task.DirectURL == "" {
		return nil, fmt.Errorf("no direct URL provided for debrid download")
	}

	// Determine filename
	fileName := task.DirectFileName
	if fileName == "" {
		fileName = task.Title
		if fileName == "" {
			fileName = task.InfoHash
		}
	}

	destPath, err := safePath(outputDir, fileName)
	if err != nil {
		return nil, fmt.Errorf("invalid filename: %w", err)
	}

	// Check for existing partial file (resume support)
	var existingSize int64
	if fi, statErr := os.Stat(destPath); statErr == nil {
		existingSize = fi.Size()
	}

	// Create cancellable context
	dlCtx, cancel := context.WithCancel(ctx)

	d.activeMu.Lock()
	d.active[task.ID] = cancel
	d.activeMu.Unlock()

	defer func() {
		d.activeMu.Lock()
		delete(d.active, task.ID)
		d.activeMu.Unlock()
		cancel()
	}()

	// Build request with optional Range header for resume
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, task.DirectURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if existingSize > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", existingSize))
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	// Handle response codes
	var totalBytes int64
	var startOffset int64

	switch resp.StatusCode {
	case http.StatusOK:
		// Full download (server doesn't support Range, or fresh start)
		if resp.ContentLength > 0 {
			totalBytes = resp.ContentLength
		}
		existingSize = 0 // Start fresh
	case http.StatusPartialContent:
		// Resume accepted
		startOffset = existingSize
		if resp.ContentLength > 0 {
			totalBytes = existingSize + resp.ContentLength
		}
	case http.StatusRequestedRangeNotSatisfiable:
		// 416 means our Range start is beyond the file size.
		// Verify local file matches the server's actual size via Content-Range header.
		if existingSize > 0 {
			if cr := resp.Header.Get("Content-Range"); cr != "" {
				// Content-Range: bytes */12345 — parse total size
				var serverSize int64
				if _, err := fmt.Sscanf(cr, "bytes */%d", &serverSize); err == nil && serverSize > 0 && existingSize != serverSize {
					// Local file size doesn't match server — re-download from scratch
					log.Printf("[%s] local size %s != server size %s, re-downloading", shortID(task.ID), formatBytes(existingSize), formatBytes(serverSize))
					existingSize = 0
					resp.Body.Close()
					req2, err := http.NewRequestWithContext(dlCtx, http.MethodGet, task.DirectURL, nil)
					if err != nil {
						return nil, fmt.Errorf("create retry request: %w", err)
					}
					resp, err = httpClient.Do(req2)
					if err != nil {
						return nil, fmt.Errorf("retry http request: %w", err)
					}
					defer resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						return nil, fmt.Errorf("retry unexpected HTTP status: %d %s", resp.StatusCode, resp.Status)
					}
					if resp.ContentLength > 0 {
						totalBytes = resp.ContentLength
					}
					break // continue to download loop
				}
			}
			log.Printf("[%s] file already complete: %s (%s)", shortID(task.ID), fileName, formatBytes(existingSize))
			return &Result{
				FilePath: destPath,
				FileName: fileName,
				Method:   MethodDebrid,
				Size:     existingSize,
			}, nil
		}
		return nil, fmt.Errorf("server returned 416 Range Not Satisfiable")
	default:
		return nil, fmt.Errorf("unexpected HTTP status: %d %s", resp.StatusCode, resp.Status)
	}

	// Open file for writing (append if resuming, create if new)
	var flags int
	if startOffset > 0 {
		flags = os.O_WRONLY | os.O_APPEND
		log.Printf("[%s] resuming debrid download at %s: %s", shortID(task.ID), formatBytes(startOffset), fileName)
	} else {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		log.Printf("[%s] starting debrid download: %s", shortID(task.ID), fileName)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return nil, fmt.Errorf("create directory: %w", err)
	}

	file, err := os.OpenFile(destPath, flags, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer func() {
		if err := file.Sync(); err != nil {
			log.Printf("[%s] sync warning: %v", shortID(task.ID), err)
		}
		file.Close()
	}()

	// Download with progress reporting
	downloaded := startOffset
	lastReportAt := time.Now()
	lastBytes := downloaded
	buf := make([]byte, 256*1024) // 256KB buffer

	for {
		select {
		case <-dlCtx.Done():
			return nil, dlCtx.Err()
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := file.Write(buf[:n]); writeErr != nil {
				return nil, fmt.Errorf("write file: %w", writeErr)
			}
			downloaded += int64(n)
		}

		// Report progress every second
		now := time.Now()
		if now.Sub(lastReportAt) >= time.Second || readErr == io.EOF {
			elapsed := now.Sub(lastReportAt).Seconds()
			var speed int64
			if elapsed > 0 {
				speed = int64(float64(downloaded-lastBytes) / elapsed)
			}

			var eta int
			if speed > 0 && totalBytes > 0 {
				eta = int((totalBytes - downloaded) / speed)
			}

			pct := 0
			if totalBytes > 0 {
				pct = int(float64(downloaded) / float64(totalBytes) * 100)
			}

			fmt.Fprintf(os.Stderr, "\r[%s] %d%% — %s/%s @ %s/s  (debrid)",
				shortID(task.ID), pct,
				formatBytes(downloaded), formatBytes(totalBytes), formatBytes(speed))

			p := Progress{
				DownloadedBytes: downloaded,
				TotalBytes:      totalBytes,
				SpeedBps:        speed,
				ETA:             eta,
				FileName:        fileName,
			}
			task.UpdateProgress(p)

			select {
			case progressCh <- p:
			default:
			}

			lastReportAt = now
			lastBytes = downloaded
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read response: %w", readErr)
		}
	}

	fmt.Fprint(os.Stderr, "\r\033[2K") // clear progress line
	log.Printf("[%s] debrid download complete: %s (%s)", shortID(task.ID), fileName, formatBytes(downloaded))

	return &Result{
		FilePath: destPath,
		FileName: fileName,
		Method:   MethodDebrid,
		Size:     downloaded,
	}, nil
}

// Pause cancels the in-progress HTTP download but keeps partial file for resume.
func (d *DebridDownloader) Pause(taskID string) error {
	d.activeMu.Lock()
	cancel, ok := d.active[taskID]
	delete(d.active, taskID)
	d.activeMu.Unlock()

	if ok {
		cancel()
		log.Printf("[%s] debrid download paused (file kept for resume)", shortID(taskID))
	}
	return nil
}

// Cancel aborts the in-progress HTTP download. Partial file is kept on disk.
func (d *DebridDownloader) Cancel(taskID string) error {
	d.activeMu.Lock()
	cancel, ok := d.active[taskID]
	delete(d.active, taskID)
	d.activeMu.Unlock()

	if ok {
		cancel()
		log.Printf("[%s] debrid download cancelled", shortID(taskID))
	}
	return nil
}

func (d *DebridDownloader) Shutdown(_ context.Context) error {
	d.activeMu.Lock()
	defer d.activeMu.Unlock()

	for id, cancel := range d.active {
		cancel()
		delete(d.active, id)
	}
	return nil
}
