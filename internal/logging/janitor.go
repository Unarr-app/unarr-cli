package logging

import (
	"context"
	"log"
	"time"
)

// DefaultSweepInterval is how often the janitor checks the log size. A minute
// is far shorter than the time a daemon needs to write a whole budget's worth
// of output, so the file never overshoots by much, and the check itself is one
// Stat — nothing a NAS will notice.
const DefaultSweepInterval = time.Minute

// Sweep keeps a FOREIGN-HELD log file inside its size budget for as long as
// ctx lives: a file whose descriptor belongs to someone else (launchd, the
// Windows shim, the detached launcher) cannot be rotated by any writer in this
// process, but RotateNow's copy-truncate can trim it from the outside while it
// is being written. A log this process owns is bounded by its Writer instead
// and must NOT be swept — two rotators on one path fight each other.
//
// Blocking; run it in a goroutine. Failures still do not stop the sweep — a
// daemon that cannot rotate its log must keep downloading — but the FIRST one
// is logged, once. Silence is how a rotation that had been failing on every
// tick went unnoticed; logging it on every tick would flood the very file that
// cannot be rotated, which is worse.
func Sweep(ctx context.Context, opts Options, every time.Duration) {
	if opts.Path == "" || opts.maxBytes() == 0 {
		return // rotation disabled — nothing to supervise
	}
	if every <= 0 {
		every = DefaultSweepInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	reported := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := RotateNow(opts); err != nil && !reported {
				reported = true
				log.Printf("[logs] rotation of %s failed and will not be retried noisily: %v", opts.Path, err)
			}
		}
	}
}
