package engine

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// The write half of a debrid download: stream the body into the .part file,
// report progress, and run the post-stream integrity gauntlet. Split out of
// debrid.go (which owns the downloader lifecycle: track/cancel/pause) so each
// file has one responsibility.

// copyPartialBody streams the response into the partial with progress reporting
// and the overlong guard. Returns the total bytes now in the partial and
// whether the body ended CLEANLY (io.EOF): a clean end far below the advertised
// length is a complete error response, while a cut stream is a valid prefix —
// the two are handled differently by persistAndCheck.
func copyPartialBody(ctx context.Context, x *debridTransfer, resp *http.Response, file *os.File, meter *progressMeter) (int64, bool, error) {
	downloaded := x.start
	buf := make([]byte, 256*1024) // 256KB buffer

	for {
		select {
		case <-ctx.Done():
			return downloaded, false, ctx.Err()
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := file.Write(buf[:n]); writeErr != nil {
				return downloaded, false, fmt.Errorf("write file: %w", writeErr)
			}
			downloaded += int64(n)
		}

		// A stream longer than the advertised length is as untrustworthy as a
		// short one (a proxy/CDN error page glued after the payload, a resume
		// against different bytes). Refuse it and start clean next attempt.
		if x.total > 0 && downloaded > x.total {
			// Artifacts are removed by the caller, AFTER it closes the file handle
			// (Windows cannot unlink an open file).
			return downloaded, false, integrityErr("overlong", "server sent more data than advertised (%s > %s)",
				formatBytes(downloaded), formatBytes(x.total))
		}

		meter.maybeReport(downloaded, readErr == io.EOF)

		if readErr == io.EOF {
			return downloaded, true, nil
		}
		if readErr != nil {
			return downloaded, false, fmt.Errorf("read response: %w", readErr)
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
func persistAndCheck(x *debridTransfer, file *os.File, closeFile func() error, downloaded int64, cleanEOF bool) error {
	outputDir := x.outputDir
	fileName := filepath.Base(x.dest)

	// Anti-stub floor: a debrid CDN answers 200 with a tiny (often all-NUL or
	// HTML) body when the link is expired / "still caching" / errored. Those
	// bytes are NOT a prefix of the real file, so keeping them as a resumable
	// partial would splice garbage under the retry — delete and restart from
	// zero, which costs nothing at this size.
	//
	// It runs BEFORE the truncation guard (which keeps the partial), and the
	// two are told apart by HOW the stream ended, not by size alone: a body the
	// server closed cleanly (io.EOF, cleanEOF) far below the advertised length
	// is a complete error response — its bytes are not a prefix of anything. A
	// stream cut mid-transfer (read error / context cancel) IS a valid prefix
	// and must be kept for resume, however few bytes arrived.
	if isVideoFile(fileName) && downloaded < minPlausibleVideoBytes && cleanEOF {
		// Close first: Windows refuses to unlink a file that is still open.
		_ = closeFile()
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
