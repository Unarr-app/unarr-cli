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

	"github.com/torrentclaw/unarr/internal/agent"
)

// httpClient is used for debrid HTTPS downloads with a reasonable header timeout.
var httpClient = &http.Client{
	Transport: &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
	},
}

// DebridDownloader downloads files via HTTPS direct URLs resolved by the server.
// The server handles all debrid provider interaction; this downloader only needs
// a plain HTTPS URL to fetch.
type DebridDownloader struct {
	activeMu sync.Mutex
	active   map[string]context.CancelFunc

	minFreeBytes int64 // disk reserve for the pre-flight space check (0 = reserve disabled)
}

// NewDebridDownloader creates a debrid downloader.
func NewDebridDownloader() *DebridDownloader {
	return &DebridDownloader{
		active: make(map[string]context.CancelFunc),
	}
}

// SetMinFreeBytes sets the free-space reserve enforced before a download starts.
// Call once at construction; 0 disables the reserve (the size-vs-free check still
// runs). See CheckDiskSpace.
func (d *DebridDownloader) SetMinFreeBytes(n int64) { d.minFreeBytes = n }

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
					log.Printf("[%s] local size %s != server size %s, re-downloading", agent.ShortID(task.ID), formatBytes(existingSize), formatBytes(serverSize))
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
			log.Printf("[%s] file already complete: %s (%s)", agent.ShortID(task.ID), fileName, formatBytes(existingSize))
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
		log.Printf("[%s] resuming debrid download at %s: %s", agent.ShortID(task.ID), formatBytes(startOffset), fileName)
	} else {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		log.Printf("[%s] starting debrid download: %s", agent.ShortID(task.ID), fileName)
	}

	// Pre-flight disk-space guard on the bytes still to write (resume subtracts
	// what's already on disk). Best-effort; ENOSPC stays the backstop.
	if err := CheckDiskSpace(outputDir, totalBytes-startOffset, d.minFreeBytes); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return nil, fmt.Errorf("create directory: %w", err)
	}

	file, err := os.OpenFile(destPath, flags, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	// Guarded close: error paths below clean up the fd via defer, while the
	// success path closes explicitly and inspects the error (a swallowed Close
	// error hides write-back failures on network mounts — the root cause of the
	// 2026-06-15 NFS truncation incident).
	closed := false
	closeFile := func() error {
		if closed {
			return nil
		}
		closed = true
		return file.Close()
	}
	defer func() { _ = closeFile() }()

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

			log.Printf("[%s] %d%% — %s/%s @ %s/s  (debrid)",
				agent.ShortID(task.ID), pct,
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

	// Guard against a premature end-of-stream: if the server advertised a length
	// and we read fewer bytes, the transfer was truncated (e.g. a debrid CDN edge
	// closing the connection). Don't hand a short file to verify as if complete.
	if totalBytes > 0 && downloaded < totalBytes {
		return nil, fmt.Errorf("incomplete download: got %s of %s", formatBytes(downloaded), formatBytes(totalBytes))
	}

	// Force the OS to flush the file to durable storage BEFORE we report success.
	// Without this, every Write() can succeed into the page cache while the actual
	// write-back to a network mount (the prod download dir is an NFS share at
	// /mnt/nas/peliculas) lags or fails — verify() then stats a half-flushed file
	// and rejects it ("size mismatch"). fsync surfaces a write-back error here,
	// where it's actionable, instead of silently truncating the file.
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("flush to disk failed (write-back/network-mount error): %w", err)
	}
	if err := closeFile(); err != nil {
		return nil, fmt.Errorf("close file failed (write-back/network-mount error): %w", err)
	}

	// Safety net: after a durable flush, the on-disk size must match what we wrote.
	// On a stalled mount a write-back error can still leave the file short even
	// when Sync/Close returned nil. This is also the ONLY integrity check when the
	// server sent no Content-Length (totalBytes == 0 → the guard above is skipped).
	// Remove the corrupt partial so a retry starts clean, rather than passing a
	// truncated file to verify().
	if fi, statErr := os.Stat(destPath); statErr == nil && fi.Size() < downloaded {
		if rmErr := os.Remove(destPath); rmErr != nil {
			log.Printf("[%s] failed to remove corrupt partial %s: %v", agent.ShortID(task.ID), destPath, rmErr)
		}
		return nil, fmt.Errorf("post-write size mismatch: wrote %s but file is %s on disk — likely a stalled or failing storage mount (%s)",
			formatBytes(downloaded), formatBytes(fi.Size()), outputDir)
	}

	log.Printf("[%s] debrid download complete: %s (%s)", agent.ShortID(task.ID), fileName, formatBytes(downloaded))

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
		log.Printf("[%s] debrid download paused (file kept for resume)", agent.ShortID(taskID))
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
		log.Printf("[%s] debrid download cancelled", agent.ShortID(taskID))
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
