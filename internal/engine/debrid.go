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
//
// Integrity model: bytes are written ONLY to "<dest>.part" with a sidecar
// recording their provenance (see partMeta); resume is validated with
// If-Range/Content-Range so two different files can never be spliced; the final
// name appears only via an atomic rename after fsync + every check passed.
type DebridDownloader struct {
	activeMu sync.Mutex
	active   map[string]context.CancelFunc
	// destPaths records the FINAL on-disk path per in-flight taskID so Cancel
	// can delete the derived partial + sidecar (torrent/usenet know their file
	// from their own handle; a debrid download is a plain HTTPS fetch, so we must
	// remember destPath here). Populated in Download once destPath is known;
	// cleared in the same defer that clears active. Pause deliberately leaves the
	// files — resume needs them.
	destPaths map[string]string
	// done is closed when a task's Download returns, i.e. once its file handle is
	// released. Cancel waits on it before unlinking: Windows refuses to delete a
	// file that is still open, so removing the partial straight after cancel() lost
	// the race and left the orphan behind.
	done map[string]chan struct{}

	// pathLocks serializes downloads per destination file. Two tasks can resolve
	// to the same DirectFileName (a re-dispatch racing a retry, two releases with
	// one filename); without this they would interleave writes into one partial.
	pathLocks *pathLocker

	minFreeBytes int64 // disk reserve for the pre-flight space check (0 = reserve disabled)
}

// NewDebridDownloader creates a debrid downloader.
func NewDebridDownloader() *DebridDownloader {
	return &DebridDownloader{
		active:    make(map[string]context.CancelFunc),
		destPaths: make(map[string]string),
		done:      make(map[string]chan struct{}),
		pathLocks: newPathLocker(),
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

// debridFileName picks the on-disk name for a task's download.
func debridFileName(task *Task) string {
	if task.DirectFileName != "" {
		return task.DirectFileName
	}
	if task.Title != "" {
		return task.Title
	}
	return task.InfoHash
}

// Download fetches the file from task.DirectURL via HTTPS with progress
// reporting. Resume via HTTP Range is validated against the partial's recorded
// provenance (If-Range + Content-Range) so it can only ever continue the same
// bytes; anything unprovable restarts clean.
func (d *DebridDownloader) Download(ctx context.Context, task *Task, outputDir string, progressCh chan<- Progress) (*Result, error) {
	if task.DirectURL == "" {
		return nil, fmt.Errorf("no direct URL provided for debrid download")
	}

	fileName := debridFileName(task)
	destPath, err := safePath(outputDir, fileName)
	if err != nil {
		return nil, fmt.Errorf("invalid filename: %w", err)
	}

	// One writer per destination: take the lock BEFORE reading the partial, so
	// the resume decision sees the state the previous writer left, not a moving
	// file being appended to by a concurrent task with the same filename.
	unlock := d.pathLocks.Lock(destPath)
	defer unlock()

	x := newDebridTransfer(task, destPath, outputDir)

	// Create cancellable context
	dlCtx, cancel := context.WithCancel(ctx)

	// Closed by the cleanup defer below, which runs AFTER the deferred closeFile
	// (defers are LIFO) — so a waiter is only released once the handle is gone.
	finished := make(chan struct{})
	d.track(task.ID, destPath, cancel, finished)
	defer func() {
		d.untrack(task.ID)
		cancel()
		close(finished)
	}()

	resp, err := x.openStream(dlCtx)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		// 416 with a matching on-disk size: the partial already holds the complete
		// file — publish it instead of re-downloading.
		return x.finalize(fileName)
	}
	defer resp.Body.Close()

	file, err := d.openPartial(x)
	if err != nil {
		return nil, err
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

	meter := newProgressMeter(x, progressCh, fileName)
	downloaded, err := copyPartialBody(dlCtx, x, resp, file, meter)
	if err != nil {
		return nil, err
	}

	if err := persistAndCheck(x, file, closeFile, downloaded); err != nil {
		return nil, err
	}

	log.Printf("[%s] debrid download complete: %s (%s)", agent.ShortID(task.ID), fileName, formatBytes(downloaded))
	return x.finalize(fileName)
}

// track/untrack maintain the in-flight bookkeeping Cancel and Pause rely on.
func (d *DebridDownloader) track(taskID, destPath string, cancel context.CancelFunc, finished chan struct{}) {
	d.activeMu.Lock()
	d.active[taskID] = cancel
	d.destPaths[taskID] = destPath
	d.done[taskID] = finished
	d.activeMu.Unlock()
}

func (d *DebridDownloader) untrack(taskID string) {
	d.activeMu.Lock()
	delete(d.active, taskID)
	delete(d.destPaths, taskID)
	delete(d.done, taskID)
	d.activeMu.Unlock()
}

// openPartial opens the .part file for writing (append when the server granted
// a resume, truncate otherwise) after the pre-flight disk-space guard.
func (d *DebridDownloader) openPartial(x *debridTransfer) (*os.File, error) {
	var flags int
	if x.start > 0 {
		flags = os.O_WRONLY | os.O_APPEND
		log.Printf("[%s] resuming debrid download at %s: %s", x.task.ShortID(), formatBytes(x.start), filepath.Base(x.dest))
	} else {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		log.Printf("[%s] starting debrid download: %s", x.task.ShortID(), filepath.Base(x.dest))
	}

	// Pre-flight disk-space guard on the bytes still to write (resume subtracts
	// what's already on disk). Best-effort; ENOSPC stays the backstop.
	if err := CheckDiskSpace(x.outputDir, x.total-x.start, d.minFreeBytes); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(x.partial), 0o755); err != nil {
		// Can't even create the target dir — the download folder is gone/read-only/
		// unmounted (a NAS that dropped, a removed drive). A StorageError, not a
		// transport failure: another method would write to the same dead folder.
		// Retry once (a mount can re-appear), then pause with a storage message.
		return nil, storageErr("mkdir_failed", x.outputDir, "could not create download folder %s — is your drive/NAS connected and writable? (%v)", filepath.Dir(x.partial), err)
	}

	file, err := os.OpenFile(x.partial, flags, 0o644)
	if err != nil {
		// Same class: the directory resolved but the file can't be opened for write
		// (permissions, read-only mount). Storage, not source.
		return nil, storageErr("open_failed", x.outputDir, "could not open the download file in %s for writing — check your folder/drive permissions (%v)", filepath.Dir(x.partial), err)
	}
	return file, nil
}

// copyPartialBody streams the response into the partial with progress reporting
// and the overlong guard. Returns the total bytes now in the partial.
func copyPartialBody(ctx context.Context, x *debridTransfer, resp *http.Response, file *os.File, meter *progressMeter) (int64, error) {
	downloaded := x.start
	buf := make([]byte, 256*1024) // 256KB buffer

	for {
		select {
		case <-ctx.Done():
			return downloaded, ctx.Err()
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := file.Write(buf[:n]); writeErr != nil {
				return downloaded, fmt.Errorf("write file: %w", writeErr)
			}
			downloaded += int64(n)
		}

		// A stream longer than the advertised length is as untrustworthy as a
		// short one (a proxy/CDN error page glued after the payload, a resume
		// against different bytes). Refuse it and start clean next attempt.
		if x.total > 0 && downloaded > x.total {
			x.removeArtifacts()
			return downloaded, integrityErr("overlong", "server sent more data than advertised (%s > %s)",
				formatBytes(downloaded), formatBytes(x.total))
		}

		meter.maybeReport(downloaded, readErr == io.EOF)

		if readErr == io.EOF {
			return downloaded, nil
		}
		if readErr != nil {
			return downloaded, fmt.Errorf("read response: %w", readErr)
		}
	}
}

// progressMeter throttles and emits download progress (log line, task update,
// non-blocking channel send) at most once per second.
type progressMeter struct {
	x         *debridTransfer
	ch        chan<- Progress
	fileName  string
	lastAt    time.Time
	lastBytes int64
}

func newProgressMeter(x *debridTransfer, ch chan<- Progress, fileName string) *progressMeter {
	return &progressMeter{x: x, ch: ch, fileName: fileName, lastAt: time.Now(), lastBytes: x.start}
}

// maybeReport emits progress when a second has passed since the last report,
// or unconditionally when force is set (end of stream).
func (m *progressMeter) maybeReport(downloaded int64, force bool) {
	now := time.Now()
	elapsed := now.Sub(m.lastAt)
	if elapsed < time.Second && !force {
		return
	}

	var speed int64
	if secs := elapsed.Seconds(); secs > 0 {
		speed = int64(float64(downloaded-m.lastBytes) / secs)
	}

	var eta int
	if speed > 0 && m.x.total > 0 {
		eta = int((m.x.total - downloaded) / speed)
	}

	pct := 0
	if m.x.total > 0 {
		pct = int(float64(downloaded) / float64(m.x.total) * 100)
	}

	log.Printf("[%s] %d%% - %s/%s @ %s/s  (debrid)",
		m.x.task.ShortID(), pct,
		formatBytes(downloaded), formatBytes(m.x.total), formatBytes(speed))

	p := Progress{
		DownloadedBytes: downloaded,
		TotalBytes:      m.x.total,
		SpeedBps:        speed,
		ETA:             eta,
		FileName:        m.fileName,
	}
	m.x.task.UpdateProgress(p)

	select {
	case m.ch <- p:
	default:
	}

	m.lastAt = now
	m.lastBytes = downloaded
}

// persistAndCheck is the post-stream gauntlet: truncation guard, durable flush,
// checked close, on-disk read-back, anti-stub floor. Only a partial that passes
// all of it gets finalized (renamed to the real name) by the caller.
func persistAndCheck(x *debridTransfer, file *os.File, closeFile func() error, downloaded int64) error {
	outputDir := x.outputDir
	fileName := filepath.Base(x.dest)

	// Anti-stub floor FIRST, before the truncation guard can "keep the partial
	// for resume": a debrid CDN answers 200 with a tiny (often all-NUL or HTML)
	// body when the link is expired / "still caching" / errored. Those bytes are
	// NOT a prefix of the real file, so a kept partial would splice garbage under
	// the resumed download. A real video is never this small — delete and retry
	// from zero, which costs nothing at this size. (With no expected size and no
	// Content-Length this is also the ONLY guard against filing the stub into the
	// library — the movie.mkv/movie (N).mkv flood in prod.)
	if isVideoFile(fileName) && downloaded < minPlausibleVideoBytes {
		x.removeArtifacts()
		return integrityErr("stub_response", "debrid returned only %s for %s — link expired or not ready (not a valid video)", formatBytes(downloaded), fileName)
	}

	// Guard against a premature end-of-stream: if the server advertised a length
	// and we read fewer bytes, the transfer was truncated (e.g. a debrid CDN edge
	// closing the connection). Don't hand a short file to verify as if complete.
	if x.total > 0 && downloaded < x.total {
		// Integrity, not transport — the manager re-downloads. Keep the partial
		// AND its sidecar: the bytes written so far are sequentially correct, so
		// the retry resumes via validated HTTP Range from where the stream was cut
		// instead of re-fetching the whole file.
		return integrityErr("truncated", "incomplete download: got %s of %s", formatBytes(downloaded), formatBytes(x.total))
	}

	// Force the OS to flush the file to durable storage BEFORE we report success.
	// Without this, every Write() can succeed into the page cache while the actual
	// write-back to a network mount (the prod download dir is an NFS share at
	// /mnt/nas/peliculas) lags or fails — verify() then stats a half-flushed file
	// and rejects it ("size mismatch"). fsync surfaces a write-back error here,
	// where it's actionable, instead of silently truncating the file.
	if err := file.Sync(); err != nil {
		_ = closeFile()
		x.removeArtifacts() // uncertain on-disk state — drop it so the retry starts clean
		// The bytes were correct; the DESTINATION failed to persist them. This is a
		// StorageError, not integrity corruption — re-downloading writes to the same
		// broken mount. The manager retries once (a mount can briefly stall) then
		// pauses as resumable with a "check your download folder / NAS" message.
		return storageErr("flush_failed", outputDir, "could not save to %s — flush to disk failed (write-back/network-mount error): %v", outputDir, err)
	}
	if err := closeFile(); err != nil {
		x.removeArtifacts()
		return storageErr("close_failed", outputDir, "could not save to %s — close file failed (write-back/network-mount error): %v", outputDir, err)
	}

	// Safety net: after a durable flush, the on-disk size must match what we wrote.
	// On a stalled mount a write-back error can still leave the file short even
	// when Sync/Close returned nil. This is also the ONLY size check when the
	// server sent no Content-Length (x.total == 0 → the guard above is skipped).
	// Remove the corrupt partial so a retry starts clean, rather than passing a
	// truncated file to verify().
	if fi, statErr := os.Stat(x.partial); statErr == nil && fi.Size() < downloaded {
		x.removeArtifacts()
		// A post-flush short file means the mount dropped the write-back even though
		// Sync/Close returned nil — a DESTINATION failure, not source corruption. Same
		// StorageError path (retry once, then pause) rather than looping re-downloads.
		return storageErr("flush_failed", outputDir, "could not save to %s — post-write size mismatch: wrote %s but file is %s on disk (likely a stalled or failing storage mount)",
			outputDir, formatBytes(downloaded), formatBytes(fi.Size()))
	}

	return nil
}

// Pause cancels the in-progress HTTP download but keeps the partial (and its
// provenance sidecar) for a validated resume.
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

// Cancel aborts the in-progress HTTP download AND removes the partial + sidecar,
// per the Downloader contract (torrent.Cancel/usenet.Cancel delete too). This is
// the method the manager's CancelAndDeleteFiles invokes; the intent is
// cancel-and-delete, so leaving the partial behind leaked orphaned .part files
// into the download dir. Pause keeps the files — it does NOT call this. We read
// destPath under the same lock that Download populates it: if the entry is still
// present the download is genuinely in-flight and its .part is safe to delete;
// if it's gone the download already finished and the completed file at destPath
// is never touched.
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

	// Delete the partial + sidecar only when we still had an in-flight destPath —
	// this is a cancel-and-delete, and the bytes on disk are an incomplete download.
	if hadPath && destPath != "" {
		for _, p := range []string{partialPath(destPath), partMetaPath(destPath)} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				// Never silence: an undeletable partial is worth a log line so it can be
				// reaped later, but it must not fail the cancel (the download is stopped).
				log.Printf("[%s] debrid cancel: failed to remove partial %s: %v", agent.ShortID(taskID), p, err)
			}
		}
		log.Printf("[%s] debrid download cancelled + partial removed", agent.ShortID(taskID))
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
