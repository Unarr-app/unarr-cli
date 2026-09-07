// Package engine — torrent_poll.go owns the once-a-second download loop:
// progress reporting, the VPN kill-switch, stall detection, and the decision
// that a download has finished.
//
// Split out of torrent.go so the tick body is a function of its own rather than
// 90 lines inside a select, and so every quantity in it is measured on ONE
// scale: the selection. See selection.completedBytes.
package engine

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/anacrolix/torrent"
	"golang.org/x/term"
)

// pollState is the bookkeeping a progress tick carries over to the next one.
type pollState struct {
	lastBytesAt    time.Time
	lastBytes      int64
	lastVPNCheckAt time.Time
	isTTY          bool
	// hashPending is the pending-hash count seen at the previous stall
	// deadline; see hashingIsProgressing.
	hashPending int
}

// piecesPendingHash counts the pieces the library still has to verify against
// the data on disk (queued for or in the initial hash). One client-lock
// acquisition via PieceStateRuns, same reason as waitPieceMarkingSettled.
func piecesPendingHash(t *torrent.Torrent) int {
	pending := 0
	for _, run := range t.PieceStateRuns() {
		if run.Checking || run.QueuedForHash {
			pending += run.Length
		}
	}
	return pending
}

// hashingIsProgressing decides, at a stall deadline with no bytes moved,
// whether the silence is the initial hash still working through the pieces.
//
// A lost or repaired piece-completion DB queues EVERY piece for the initial
// hash, index-ordered, and queued pieces are not requested from peers, so the
// selected file's bytes stay flat for as long as the verify takes — longer
// than the stall timeout for a big pack on a slow disk. That is progress, not
// a stall. But a hash that is itself wedged (a pieceHasher blocked in a page
// fault on an NFS mount that dropped) must not hold a manager slot forever:
// with MaxTimeout=0 in every caller this is the only bound on a task. So the
// exemption is granted only while the pending count is FALLING between
// consecutive deadlines; a count that stands still for a whole StallTimeout is
// a stall.
func (st *pollState) hashingIsProgressing(t *torrent.Torrent) bool {
	pending := piecesPendingHash(t)
	progressing := pending > 0 && (st.hashPending == 0 || pending < st.hashPending)
	st.hashPending = pending
	return progressing
}

func (d *TorrentDownloader) pollDownload(ctx context.Context, t *torrent.Torrent, task *Task, sel selection, progressCh chan<- Progress) (*Result, error) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// MaxTimeout = 0 means unlimited (like qBittorrent)
	var deadline <-chan time.Time
	if d.cfg.MaxTimeout > 0 {
		deadline = time.After(d.cfg.MaxTimeout)
	}
	st := pollState{
		lastBytesAt:    time.Now(),
		lastVPNCheckAt: time.Now(),
		isTTY:          term.IsTerminal(int(os.Stderr.Fd())),
	}

	for {
		select {
		case <-ctx.Done():
			st.endProgressLine()
			return nil, fmt.Errorf("cancelled")

		case <-deadline:
			st.endProgressLine()
			return nil, fmt.Errorf("max timeout %s exceeded", d.cfg.MaxTimeout)

		case <-ticker.C:
			done, err := d.progressTick(t, task, sel, &st, progressCh)
			if err != nil {
				return nil, err
			}
			if done {
				log.Printf("[%s] download complete: %s", task.ShortID(), sel.fileName)
				return &Result{}, nil
			}
		}
	}
}

// progressTick is one second of the download loop. It reports progress, enforces
// the VPN kill-switch and the stall timeout, and returns done=true once the
// SELECTION is fully verified on disk.
func (d *TorrentDownloader) progressTick(
	t *torrent.Torrent,
	task *Task,
	sel selection,
	st *pollState,
	progressCh chan<- Progress,
) (bool, error) {
	now := time.Now()
	// Every byte figure below is selection-scoped, matching sel.totalBytes:
	// t.BytesCompleted() would count pieces of files we never selected and both
	// overstate progress and cross the completion line early.
	downloaded := sel.completedBytes(t)

	// Kill-switch: if the VPN went down mid-download, stop now and return
	// ErrVPNTunnelDown. The caller drops the torrent (partial files kept =
	// paused), so no peer/tracker traffic continues without the tunnel.
	if !d.vpnStillHealthy(&st.lastVPNCheckAt, now) {
		st.endProgressLine()
		log.Printf("[%s] VPN tunnel went down - pausing torrent (files kept, P2P disabled)", task.ShortID())
		return false, ErrVPNTunnelDown
	}

	speed := downloaded - st.lastBytes
	if speed < 0 {
		speed = 0
	}

	// Stall detection (0 = disabled, like qBittorrent)
	if downloaded > st.lastBytes {
		st.lastBytesAt = now
		st.lastBytes = downloaded
	} else if d.cfg.StallTimeout > 0 && now.Sub(st.lastBytesAt) > d.cfg.StallTimeout {
		if st.hashingIsProgressing(t) {
			st.lastBytesAt = now
		} else {
			stats := t.Stats()
			st.endProgressLine()
			return false, fmt.Errorf("stalled: no progress for %s (peers: %d, seeds: %d)",
				d.cfg.StallTimeout, stats.ActivePeers, stats.ConnectedSeeders)
		}
	}

	var eta int
	if speed > 0 {
		eta = int((sel.totalBytes - downloaded) / speed)
	}

	stats := t.Stats()
	st.logProgress(task, downloaded, sel.totalBytes, speed, stats)

	p := Progress{
		DownloadedBytes: downloaded,
		TotalBytes:      sel.totalBytes,
		SpeedBps:        speed,
		ETA:             eta,
		Peers:           stats.ActivePeers,
		Seeds:           stats.ConnectedSeeders,
		FileName:        sel.fileName,
	}
	task.UpdateProgress(p)

	select {
	case progressCh <- p:
	default: // don't block if channel full
	}

	if downloaded >= sel.totalBytes {
		st.endProgressLine()
		return true, nil
	}
	return false, nil
}

// logProgress writes the one-line progress report.
//
// ASCII only: this line goes to log.Print, i.e. into unarr.log, which a Windows
// console (code page 437/850) and a CP1252 reader both decode byte-wise. The em
// dash that used to be here reached users' logs and the crash reports as the
// bytes C7 F6; a field report shows the run of them. See
// internal/logging.TestLogLinesAreASCII.
func (st *pollState) logProgress(task *Task, downloaded, totalBytes, speed int64, stats torrent.TorrentStats) {
	var pct int
	if totalBytes > 0 {
		pct = int(float64(downloaded) / float64(totalBytes) * 100)
	}
	line := fmt.Sprintf("[%s] %d%% - %s/%s @ %s/s  peers:%d seeds:%d",
		task.ShortID(), pct,
		formatBytes(downloaded), formatBytes(totalBytes), formatBytes(speed),
		stats.ActivePeers, stats.ConnectedSeeders)
	if st.isTTY {
		fmt.Fprintf(os.Stderr, "\r\033[K%s", line)
	} else {
		log.Print(line)
	}
}

// endProgressLine closes the in-place "\r" progress line before anything else is
// printed. No-op when stderr is not a terminal (each report was its own log line).
func (st *pollState) endProgressLine() {
	if st.isTTY {
		fmt.Fprintln(os.Stderr)
	}
}
