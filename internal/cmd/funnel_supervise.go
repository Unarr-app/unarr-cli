package cmd

// Supervision for the CloudFlare Quick Tunnel, moved out of daemon.go so the
// retry policy — the part that decides how much a failing funnel is allowed to
// say — lives somewhere it can be read and tested on its own.

import (
	"context"
	"errors"
	"log"
	"sync/atomic"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/funnel"
)

const (
	funnelInitialBackoff = 2 * time.Second
	funnelMaxBackoff     = 5 * time.Minute
	// funnelHealthyRun is how long a tunnel must live — AND have published a
	// URL — to count as healthy and reset the backoff. Without the reset the
	// backoff only ever grew: a few flaky restarts early in the daemon's life
	// ratcheted it toward 5 min, and then EVERY later rotation of a perfectly
	// healthy tunnel cost remote viewers up to 5 minutes of downtime, forever.
	// The URL requirement matters: a cloudflared that connects but never
	// provisions (a regional CF incident) idles past any lifetime threshold
	// before dying, and lifetime alone would call that healthy and churn
	// max-frequency restarts through the whole incident.
	funnelHealthyRun = 5 * time.Minute
	// funnelQuietAfter is how many CONSECUTIVE start failures are logged in
	// full before the supervisor goes quiet about them.
	//
	// The tunnel is a nice-to-have; the log is shared with everything else the
	// daemon has to say, and it is the evidence a crash report carries. A
	// permanently-failing funnel used to emit a line every five minutes — 288 a
	// day, indefinitely — which is how a real field report arrived with its DHT
	// and funnel bookkeeping intact and nothing else. After this many failures
	// the supervisor keeps trying (a CF outage does end) but says so only once
	// per funnelQuietInterval.
	funnelQuietAfter = 3
	// funnelQuietInterval is how often a still-failing funnel is allowed to
	// repeat itself once it has gone quiet. Long enough to be nearly free in the
	// log, short enough that an operator reading it sees the state is current.
	funnelQuietInterval = 6 * time.Hour
)

// superviseFunnel keeps a CloudFlare Quick Tunnel up across cloudflared crashes
// and CF's ~6h tunnel rotation. On a clean exit (cancellation) it returns; on a
// crash it clears the reported URL and respawns with an exponential backoff so a
// cloudflared that cannot reach the CF edge is not hammered into a tight loop.
//
// It distinguishes two kinds of failure, because only one of them is worth
// retrying at all:
//
//   - ErrNoAutoDownload — no cloudflared on the box and no way to fetch one
//     here. Nothing the daemon does changes that; only a human installing the
//     binary does. The supervisor logs one actionable line and STOPS.
//   - everything else — a CF incident, a transient spawn failure, the routine
//     6h rotation. These do resolve on their own, so it keeps retrying, and
//     only its VOICE is rate-limited.
func superviseFunnel(ctx context.Context, d *agent.Daemon, port int) {
	backoff := funnelInitialBackoff
	failures := 0
	var lastQuietLog time.Time

	for ctx.Err() == nil {
		t, err := funnel.Start(ctx, funnel.Config{Port: port})
		if err != nil {
			// Unfixable by waiting: say it once, clearly, and stop. Restarting
			// the daemon (or `unarr daemon restart` after installing
			// cloudflared) is what picks it up again — which is exactly what the
			// message tells the user to do.
			if errors.Is(err, funnel.ErrNoAutoDownload) {
				log.Printf("[funnel] disabled for this run: %v", err)
				log.Printf("[funnel] restart the agent once cloudflared is installed, " +
					"or set [funnel] enabled = false to stop trying")
				return
			}
			failures++
			logFunnelFailure(err, failures, backoff, &lastQuietLog)
			if !funnelSleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, funnelMaxBackoff)
			continue
		}
		failures = 0
		lastQuietLog = time.Time{}

		healthy, alive := runTunnel(ctx, d, t)
		if !alive {
			return
		}
		if healthy {
			backoff = funnelInitialBackoff
		}
		if !funnelSleep(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, funnelMaxBackoff)
	}
}

// runTunnel owns one cloudflared process: publish its URL, wait for it to exit,
// and report back. `healthy` means the run earned a backoff reset (it lived
// long enough AND actually published a URL); `alive` is false when the daemon
// itself is going away and supervision should stop.
func runTunnel(ctx context.Context, d *agent.Daemon, t *funnel.Tunnel) (healthy, alive bool) {
	startedAt := time.Now()
	var gotURL atomic.Bool
	log.Printf("[funnel] cloudflared started, waiting for public URL...")
	go func() {
		url, werr := t.WaitURL(45 * time.Second)
		if werr != nil {
			log.Printf("[funnel] cloudflared did not emit a URL (%v)", werr)
			return
		}
		log.Printf("[funnel] public URL: %s", url)
		gotURL.Store(true)
		d.SetFunnelURL(url)
	}()

	// Block until cloudflared exits (CF rotation, crash, or shutdown).
	exitErr := <-t.Done()
	_ = t.Close()
	d.SetFunnelURL("")
	if ctx.Err() != nil {
		return false, false
	}
	if exitErr != nil {
		log.Printf("[funnel] cloudflared exited: %v - restarting", exitErr)
	} else {
		log.Printf("[funnel] cloudflared exited cleanly - restarting")
	}
	return gotURL.Load() && time.Since(startedAt) >= funnelHealthyRun, true
}

// logFunnelFailure emits a start failure at a volume that matches how much new
// information it carries. The first few are logged in full; after that the same
// failure repeating every few minutes is summarised at most once per
// funnelQuietInterval, with the count so nobody has to guess how long it has
// been broken.
//
// The zero lastQuietLog makes the FIRST summary fire the moment the threshold
// is crossed, and that is deliberate: a log that simply stops mentioning a
// broken subsystem reads as a subsystem that recovered. The transition line
// says, once, that the funnel is still failing and that the log is about to go
// quiet about it.
func logFunnelFailure(err error, failures int, backoff time.Duration, lastQuietLog *time.Time) {
	if failures <= funnelQuietAfter {
		log.Printf("[funnel] could not start CloudFlare tunnel (%v) - retrying in %s", err, backoff)
		return
	}
	if time.Since(*lastQuietLog) < funnelQuietInterval {
		return
	}
	*lastQuietLog = time.Now()
	log.Printf("[funnel] still cannot start CloudFlare tunnel after %d attempts (%v) - "+
		"still retrying every %s, and staying quiet about it", failures, err, backoff)
}

// funnelSleep waits for d or for cancellation. False means "the daemon is going
// away, stop supervising".
func funnelSleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
