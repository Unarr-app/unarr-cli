package cmd

import (
	"log"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// resumeInterrupted re-queues the downloads a previous run left unfinished, in
// the order the store returns them (oldest first). Each downloader picks up its
// partial data (torrent via the piece-completion DB, debrid via Range, usenet
// via its tracker).
//
// Re-submission is unconditional on purpose (the server may be unreachable at
// boot, and a resumable download must not depend on that), but it is not
// unaccountable: a task the server has forgotten gets reaped after
// unknownTaskThreshold status reports (see ProgressReporter), and `unarr
// downloads purge` drops the whole queue by hand.
//
// Two exceptions:
//   - a task the user PAUSED stays paused — restorePaused puts it back in the
//     pause set so `unarr downloads` lists it and a resume re-runs it. Before,
//     a restart silently re-ran every pause (2026-09-23: three paused torrents
//     held three of five download slots).
//   - ForceStart is dropped: a bulk auto-resume respects MaxConcurrent.
//
// submit must not block (Manager.Submit queues); this runs before the first
// sync, and a blocking submit is what once kept the daemon from ever syncing.
func resumeInterrupted(tasks []agent.Task, submit func(agent.Task), restorePaused func(taskID string)) {
	if len(tasks) == 0 {
		return
	}
	paused := 0
	for _, t := range tasks {
		if t.ResumePaused {
			paused++
		}
	}
	log.Printf("[resume] %d interrupted download(s): re-queuing %d, keeping %d paused - any the server no longer knows about will be dropped after a few status reports",
		len(tasks), len(tasks)-paused, paused)
	for _, t := range tasks {
		if t.ResumePaused {
			log.Printf("[resume] %s - %s (paused, not started)", agent.ShortID(t.ID), t.Title)
			restorePaused(t.ID)
			continue
		}
		t.ForceStart = false
		log.Printf("[resume] %s - %s", agent.ShortID(t.ID), t.Title)
		submit(t)
	}
}
