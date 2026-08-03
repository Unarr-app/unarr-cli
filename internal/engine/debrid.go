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

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// cancelDrainTimeout bounds how long Cancel waits for the download goroutine to
// release the partial's file handle before unlinking it anyway.
const cancelDrainTimeout = 10 * time.Second

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
	// destPaths records the on-disk partial path per in-flight taskID so Cancel
	// can delete it (torrent/usenet know their file from their own handle; a
	// debrid download is a plain HTTPS fetch, so we must remember destPath here).
	// Populated in Download once destPath is known; cleared in the same defer that
	// clears active. Pause deliberately leaves the file — resume needs it.
	destPaths map[string]string
	// done is closed when a task's Download returns, i.e. once its file handle is
	// released. Cancel waits on it before unlinking: Windows refuses to delete a
	// file that is still open, so removing the partial straight after cancel() lost
	// the race and left the orphan behind.
	done map[string]chan struct{}

	minFreeBytes int64 // disk reserve for the pre-flight space check (0 = reserve disabled)
}

// NewDebridDownloader creates a debrid downloader.
func NewDebridDownloader() *DebridDownloader {
	return &DebridDownloader{
		active:    make(map[string]context.CancelFunc),
		destPaths: make(map[string]string),
		done:      make(map[string]chan struct{}),
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

	// Closed by the cleanup defer below, which runs AFTER the deferred closeFile
	// (defers are LIFO) — so a waiter is only released once the handle is gone.
	finished := make(chan struct{})

	d.activeMu.Lock()
	d.active[task.ID] = cancel
	// Remember the partial path so a cancel-and-delete can remove it. Pause never
	// consults this map, so pausing keeps the file for resume.
	d.destPaths[task.ID] = destPath
	d.done[task.ID] = finished
	d.activeMu.Unlock()

	defer func() {
		d.activeMu.Lock()
		delete(d.active, task.ID)
		delete(d.destPaths, task.ID)
		delete(d.done, task.ID)
		d.activeMu.Unlock()
		cancel()
		close(finished)
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
		// Can't even create the target dir — the download folder is gone/read-only/
		// unmounted (a NAS that dropped, a removed drive). A StorageError, not a
		// transport failure: another method would write to the same dead folder.
		// Retry once (a mount can re-appear), then pause with a storage message.
		return nil, storageErr("mkdir_failed", outputDir, "could not create download folder %s — is your drive/NAS connected and writable? (%v)", filepath.Dir(destPath), err)
	}

	file, err := os.OpenFile(destPath, flags, 0o644)
	if err != nil {
		// Same class: the directory resolved but the file can't be opened for write
		// (permissions, read-only mount). Storage, not source.
		return nil, storageErr("open_failed", outputDir, "could not open the download file in %s for writing — check your folder/drive permissions (%v)", filepath.Dir(destPath), err)
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
		// Integrity, not transport — the manager re-downloads. Keep the partial
		// (NOT removed): the bytes written so far are sequentially correct, so the
		// retry resumes via HTTP Range from where the stream was cut instead of
		// re-fetching the whole file.
		return nil, integrityErr("truncated", "incomplete download: got %s of %s", formatBytes(downloaded), formatBytes(totalBytes))
	}

	// Force the OS to flush the file to durable storage BEFORE we report success.
	// Without this, every Write() can succeed into the page cache while the actual
	// write-back to a network mount (the prod download dir is an NFS share at
	// /mnt/nas/peliculas) lags or fails — verify() then stats a half-flushed file
	// and rejects it ("size mismatch"). fsync surfaces a write-back error here,
	// where it's actionable, instead of silently truncating the file.
	if err := file.Sync(); err != nil {
		_ = closeFile()
		_ = os.Remove(destPath) // uncertain on-disk state — drop it so the retry starts clean
		// The bytes were correct; the DESTINATION failed to persist them. This is a
		// StorageError, not integrity corruption — re-downloading writes to the same
		// broken mount. The manager retries once (a mount can briefly stall) then
		// pauses as resumable with a "check your download folder / NAS" message.
		return nil, storageErr("flush_failed", outputDir, "could not save to %s — flush to disk failed (write-back/network-mount error): %v", outputDir, err)
	}
	if err := closeFile(); err != nil {
		_ = os.Remove(destPath)
		return nil, storageErr("close_failed", outputDir, "could not save to %s — close file failed (write-back/network-mount error): %v", outputDir, err)
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
		// A post-flush short file means the mount dropped the write-back even though
		// Sync/Close returned nil — a DESTINATION failure, not source corruption. Same
		// StorageError path (retry once, then pause) rather than looping re-downloads.
		return nil, storageErr("flush_failed", outputDir, "could not save to %s — post-write size mismatch: wrote %s but file is %s on disk (likely a stalled or failing storage mount)",
			outputDir, formatBytes(downloaded), formatBytes(fi.Size()))
	}

	// Anti-stub floor: a debrid CDN answers 200 with no Content-Length and a tiny
	// (often all-NUL) body when the link is expired / "still caching" / errored.
	// With totalBytes==0 the truncation guard above is skipped, so such a body would
	// otherwise sail through verify() as a "complete" download and organize() would
	// file it into the library as a movie — the root cause of the movie.mkv/movie (N).mkv
	// stub flood in prod. A real video file is never this small; reject it as an
	// integrity failure and remove the stub so the manager gives up cleanly instead
	// of accreting one version-tagged sibling per failed resolve.
	if isVideoFile(fileName) && downloaded < minPlausibleVideoBytes {
		if rmErr := os.Remove(destPath); rmErr != nil {
			log.Printf("[%s] failed to remove stub download %s: %v", agent.ShortID(task.ID), destPath, rmErr)
		}
		return nil, integrityErr("stub_response", "debrid returned only %s for %s — link expired or not ready (not a valid video)", formatBytes(downloaded), fileName)
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

// Cancel aborts the in-progress HTTP download AND removes the partial file, per
// the Downloader contract (torrent.Cancel/usenet.Cancel delete too). This is the
// method the manager's CancelAndDeleteFiles invokes; the intent is cancel-and-delete,
// so leaving the partial behind (as the old debrid Cancel did) leaked orphaned
// .part-style files into the download dir. Pause keeps the file — it does NOT call
// this. We read destPath under the same lock that Download populates it: if the
// entry is still present the download is genuinely in-flight and destPath is a
// partial safe to delete; if it's gone the download already finished and we must
// NOT touch what may be a completed file.
func (d *DebridDownloader) Cancel(taskID string) error {
	d.activeMu.Lock()
	cancel, ok := d.active[taskID]
	destPath, hadPath := d.destPaths[taskID]
	finished := d.done[taskID]
	delete(d.active, taskID)
	delete(d.destPaths, taskID)
	d.activeMu.Unlock()

	if ok {
		cancel()
	}

	// Wait for the download goroutine to release the file before unlinking:
	// Windows refuses to delete an open file, so an immediate Remove here left the
	// partial on disk. Bounded so a wedged transfer can't block the cancel path.
	if finished != nil {
		select {
		case <-finished:
		case <-time.After(cancelDrainTimeout):
			log.Printf("[%s] debrid cancel: download did not stop within %s; removing partial anyway", agent.ShortID(taskID), cancelDrainTimeout)
		}
	}

	// Delete the partial only when we still had an in-flight destPath — this is a
	// cancel-and-delete, and the bytes on disk are an incomplete download.
	if hadPath && destPath != "" {
		if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
			// Never silence: an undeletable partial is worth a log line so it can be
			// reaped later, but it must not fail the cancel (the download is stopped).
			log.Printf("[%s] debrid cancel: failed to remove partial %s: %v", agent.ShortID(taskID), destPath, err)
		} else {
			log.Printf("[%s] debrid download cancelled + partial removed", agent.ShortID(taskID))
		}
	} else if ok {
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
		// Shutdown keeps partials on disk (like Pause) so a daemon restart can resume;
		// just drop the bookkeeping entry.
		delete(d.destPaths, id)
	}
	return nil
}
