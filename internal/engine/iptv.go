package engine

import (
	"context"
	"log"
	"sync"
	"sync/atomic"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// IptvDownloader downloads IPTV (Xtream) VOD files. The server mints the
// provider URL at claim time (credentials never live in the task row), so the
// transfer itself is the same validated, resumable HTTP fetch as debrid. Two
// IPTV rules sit on top of it:
//
//   - one at a time: an IPTV account usually allows a single connection, so a
//     second IPTV task waits its turn;
//   - playback first: while the user watches IPTV (PlaybackHold) the transfer
//     stops — partial kept — and resumes with Range when playback ends.
//
// While it waits for either, the task gives its download slot back (the same
// mechanism as a stalled torrent), so the rest of the queue keeps moving.
type IptvDownloader struct {
	http *DebridDownloader
	hold *PlaybackHold
	turn chan struct{} // capacity 1: the single IPTV connection

	// dests maps each task this downloader has started to its final path. The
	// HTTP downloader forgets a task once its attempt returns, and the manager
	// cancels a task's context BEFORE calling Cancel — so a cancel-and-delete
	// during a playback hold (or right as an attempt unwinds) would find nothing
	// to delete. The entry therefore outlives the Download call when that ended
	// by cancellation (pause, cancel, shutdown), and is dropped only on a
	// terminal outcome or by Cancel itself.
	destsMu sync.Mutex
	dests   map[string]string
}

// NewIptvDownloader returns an IPTV downloader driven by the given hold.
func NewIptvDownloader(hold *PlaybackHold) *IptvDownloader {
	return &IptvDownloader{
		http:  NewDebridDownloader(),
		hold:  hold,
		turn:  make(chan struct{}, 1),
		dests: make(map[string]string),
	}
}

// SetMinFreeBytes sets the free-space reserve enforced before a transfer starts.
func (d *IptvDownloader) SetMinFreeBytes(n int64) { d.http.SetMinFreeBytes(n) }

func (d *IptvDownloader) Method() DownloadMethod { return MethodIPTV }

// Available: only tasks the server created as IPTV, once it minted their URL.
func (d *IptvDownloader) Available(_ context.Context, task *Task) (bool, error) {
	return task.PreferredMethod == string(MethodIPTV) && task.DirectURL != "", nil
}

// Download runs the transfer in attempts: each one ends either with the file,
// with a real error, or because playback started — then it waits and resumes.
func (d *IptvDownloader) Download(ctx context.Context, task *Task, outputDir string, progressCh chan<- Progress) (*Result, error) {
	if err := d.takeTurn(ctx, task); err != nil {
		return nil, err
	}
	defer func() { <-d.turn }()
	d.track(task, outputDir)
	for {
		if err := d.waitPlayback(ctx, task); err != nil {
			return nil, err // cancelled: keep the dest for a following Cancel
		}
		res, interrupted, err := d.attempt(ctx, task, outputDir, progressCh)
		if !interrupted {
			if ctx.Err() == nil {
				d.untrack(task.ID)
			}
			if res != nil {
				res.Method = MethodIPTV // the shared HTTP transfer stamps debrid
			}
			return res, redactURL(err, task.DirectURL)
		}
		log.Printf("[%s] iptv: playback started - download paused, it resumes when playback ends", task.ShortID())
	}
}

// takeTurn waits for the single IPTV connection, off the download slot.
func (d *IptvDownloader) takeTurn(ctx context.Context, task *Task) error {
	select {
	case d.turn <- struct{}{}:
		return nil
	default:
	}
	log.Printf("[%s] iptv: waiting for the running IPTV download to finish (one connection per account)", task.ShortID())
	task.YieldSlot()
	defer task.ReclaimSlot()
	select {
	case d.turn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitPlayback waits, off the download slot, while IPTV is playing.
func (d *IptvDownloader) waitPlayback(ctx context.Context, task *Task) error {
	if !d.hold.Held() {
		return nil
	}
	log.Printf("[%s] iptv: IPTV is playing - download waits until playback ends", task.ShortID())
	task.YieldSlot()
	defer task.ReclaimSlot()
	if err := d.hold.WaitReleased(ctx); err != nil {
		return err
	}
	log.Printf("[%s] iptv: playback ended - resuming download", task.ShortID())
	return nil
}

// attempt runs one transfer, cut short (interrupted=true) when playback starts.
func (d *IptvDownloader) attempt(ctx context.Context, task *Task, outputDir string, progressCh chan<- Progress) (*Result, bool, error) {
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var interrupted atomic.Bool
	go func() {
		if d.hold.WaitHeld(attemptCtx) == nil {
			interrupted.Store(true)
			cancel()
		}
	}()
	res, err := d.http.Download(attemptCtx, task, outputDir, progressCh)
	if err == nil || ctx.Err() != nil || !interrupted.Load() {
		return res, false, err
	}
	return nil, true, err
}

func (d *IptvDownloader) track(task *Task, outputDir string) {
	dest, err := safePath(outputDir, debridFileName(task))
	if err != nil {
		return
	}
	d.destsMu.Lock()
	d.dests[task.ID] = dest
	d.destsMu.Unlock()
}

func (d *IptvDownloader) untrack(taskID string) string {
	d.destsMu.Lock()
	defer d.destsMu.Unlock()
	dest := d.dests[taskID]
	delete(d.dests, taskID)
	return dest
}

// Pause stops the transfer and keeps the partial for a later resume.
func (d *IptvDownloader) Pause(taskID string) error { return d.http.Pause(taskID) }

// Cancel aborts and removes the partial — including one parked by a hold, and
// one whose attempt already unwound when the manager cancelled its context.
func (d *IptvDownloader) Cancel(taskID string) error {
	err := d.http.Cancel(taskID)
	if dest := d.untrack(taskID); dest != "" {
		removePartialArtifacts(agent.ShortID(taskID), dest)
	}
	return err
}

// Shutdown stops every transfer; partials stay for the next start.
func (d *IptvDownloader) Shutdown(ctx context.Context) error { return d.http.Shutdown(ctx) }
