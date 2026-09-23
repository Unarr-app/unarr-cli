package engine

import (
	"context"
	"errors"
	"log"
	"time"
)

// defaultMetadataStallAfter is how long a torrent may wait for its metadata
// while holding a download slot. Past it, the task yields the slot (see
// slotLease) and keeps waiting — the metadata timeout, 0 = unlimited by
// default, still decides when it gives up. 30 min is Transmission's
// queue-stalled-minutes default: long enough for a slow swarm to answer, short
// enough that a dead magnet cannot hold the queue for days.
const defaultMetadataStallAfter = 30 * time.Minute

var (
	errMetadataTimeout   = errors.New("metadata timeout")
	errMetadataCancelled = errors.New("cancelled while waiting for metadata")
)

// awaitMetadata blocks until gotInfo fires (nil), the metadata timeout expires
// (errMetadataTimeout) or ctx ends (errMetadataCancelled). After the stall
// threshold it yields the task's download slot, and reclaims it if metadata
// arrives or the timeout hands the task to a fallback method afterwards.
func (d *TorrentDownloader) awaitMetadata(ctx context.Context, gotInfo <-chan struct{}, task *Task) error {
	var timeout <-chan time.Time
	if d.cfg.MetadataTimeout > 0 {
		tm := time.NewTimer(d.cfg.MetadataTimeout)
		defer tm.Stop()
		timeout = tm.C
	}
	var stall <-chan time.Time
	if after := d.metadataStallAfter(); after > 0 {
		st := time.NewTimer(after)
		defer st.Stop()
		stall = st.C
	}

	yielded := false
	for {
		select {
		case <-gotInfo:
			if yielded {
				task.ReclaimSlot()
				log.Printf("[%s] metadata arrived after stalling - counts against the download slots again", task.ShortID())
			}
			return nil
		case <-stall:
			stall = nil
			yielded = true
			log.Printf("[%s] no metadata after %s - releasing its download slot to the queue (still looking for peers)",
				task.ShortID(), d.metadataStallAfter())
			task.YieldSlot()
		case <-timeout:
			if yielded {
				// The manager may try another method next (debrid/usenet), and that
				// download must count against the cap like any other.
				task.ReclaimSlot()
			}
			return errMetadataTimeout
		case <-ctx.Done():
			return errMetadataCancelled
		}
	}
}

// metadataStallAfter resolves the configured stall threshold: 0 means the
// default, a negative value disables yielding.
func (d *TorrentDownloader) metadataStallAfter() time.Duration {
	if d.cfg.MetadataStallAfter == 0 {
		return defaultMetadataStallAfter
	}
	return d.cfg.MetadataStallAfter
}
