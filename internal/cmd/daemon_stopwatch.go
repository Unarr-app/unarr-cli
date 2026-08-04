package cmd

import (
	"context"
	"log"
	"os"
	"syscall"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// stopIntentPoll is how often the daemon checks whether a stop was requested.
// Two seconds is imperceptible to a user pressing Pause and costs one stat().
// A var so tests can shorten it; nothing else may write it.
var stopIntentPoll = 2 * time.Second

// watchStopIntent makes the daemon stop itself when someone asks it to, by
// noticing the stop-intent marker appear on disk.
//
// Windows has no signals, so `unarr stop` there is taskkill /f against whatever
// PID the state file names — and that is not good enough for two measured
// reasons:
//
//   - The state file is written during registration and SURVIVES a crash, so in
//     the window between the launcher shim relaunching a crashed daemon and the
//     new one registering, it names the previous, dead PID. Stop then killed
//     nothing while a live daemon carried on.
//   - Ending the scheduled task does not help: it takes down wscript.exe, but
//     the unarr.exe grandchild SURVIVES as an orphan (measured on real Windows —
//     the daemon was still up six seconds after `schtasks /end`).
//
// Whoever is running does not need to be found, though: it can be told. The
// marker is a fact on disk, so any daemon — the one the state file knows about
// or the one it does not — sees it within a poll and shuts down through the
// ordinary signal path: drain, deregister, seal, reap. That also makes a Windows
// stop GRACEFUL for the first time; taskkill /f never let downloads drain.
//
// Feeding the existing signal channel rather than duplicating the shutdown block
// is deliberate: there is exactly one shutdown sequence, and it stays that way.
//
// The daemon clears the marker at startup, so only a marker that appears AFTER
// this daemon came up is a request aimed at it.
func watchStopIntent(ctx context.Context, sigCh chan<- os.Signal) {
	ticker := time.NewTicker(stopIntentPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !agent.StopIntentExists() {
				continue
			}
			log.Printf("[agent] stop requested (%s) - shutting down", agent.StopIntentPath())
			select {
			case sigCh <- syscall.SIGTERM:
			default: // a shutdown is already under way
			}
			return
		}
	}
}
