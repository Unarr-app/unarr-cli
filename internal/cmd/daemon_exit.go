package cmd

import "github.com/Unarr-app/unarr-cli/internal/agent"

// runDaemon is the entry point behind every command that brings the daemon up
// (`unarr start`, `unarr up`). It exists to make one guarantee runDaemonStart
// cannot make for itself: a DELIBERATE exit must not masquerade as a crash.
//
// The daemon state file is written mid-startup, by register(), long before the
// agent is actually serving — and until now only the signal path removed it
// again. Every other way out left a file saying "status: running" next to a PID
// that no longer exists: a fatal startup error, and the credential-revoked
// shutdown the daemon itself documents as "clean, expected exit … not a crash".
// That pair is exactly what unarr-desktop reads as a death, so those clean exits
// were notifying the user and mailing a crash report to the developers.
//
// The reap is a plain call and NOT a defer, which is the entire design:
//
//   - a return (clean stop, revoked credential, fatal startup error) reaches it
//     → the state file goes, and nothing reports a crash that did not happen;
//   - a panic unwinds straight past it → the state file stays, and a genuine
//     in-process crash is still caught and reported, which is what the crash
//     report is FOR.
//
// Deferring it would quietly delete the evidence for the one case the whole
// mechanism exists to catch. See agent.ReapOwnState for the PID guard that keeps
// a dev agent and the production agent from reaping each other.
func runDaemon() error {
	err := runDaemonStart()
	// Seal BEFORE reaping. Removing the file is not enough on its own: the sync
	// loop and the task reporters are still unwinding at this point, and a single
	// straggling WriteState re-creates it from the in-memory snapshot — "status":
	// "running", heartbeat from before the stop — which is exactly the pair the
	// tray reports as a crash. Sealing first means nothing can undo the reap.
	agent.SealState()
	agent.ReapOwnState()
	return err
}
