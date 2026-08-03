package cmd

import (
	"errors"
	"fmt"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/service"
)

// Stopping the daemon, and cleaning up after a daemon that stopped.
//
// Split out of daemon_control.go (which is about driving the service manager)
// because "make it stop, make it STAY stopped, and leave no wreckage that reads
// as a crash" is its own responsibility, with its own hard-won rules: a stop
// must be recorded as deliberate before the process dies, and the state file it
// leaves behind must be reaped or the tray reports a crash that never happened.

// runStop is what `unarr stop` does: stop the daemon and have it STAY stopped.
//
// Under a respawning supervisor (systemd Restart=always, launchd KeepAlive)
// signalling the PID is not a stop: the unit comes back RestartSec later. That
// is exactly what a tray "Pause" looked like to users — the agent went green
// again ~10s after they paused it — because the tray also ran this command. So
// delegate to the service manager whenever one is installed, and keep the PID
// path only for a daemon started in the foreground.
func runStop() error {
	if service.Respawns() {
		return runDaemonSvcStop()
	}
	return stopDaemonByPID()
}

// stopDaemonByPID stops the daemon: intent first, then the supervisor, then the
// PID. Used on platforms without a service manager — which is Windows, plus a
// foreground daemon anywhere.
//
// The first two steps are UNCONDITIONAL, and that ordering is the fix for a
// window measured on real Windows. `unarr stop` finds its target in the state
// file, which is written during registration (seconds into startup) and survives
// a crash — so between the shim relaunching a crashed daemon and that new daemon
// registering, the file still names the previous, dead PID. Deciding what to do
// from that file alone meant taking the "already dead" branch and killing
// nothing while a live daemon carried on. Recording the intent and ending the
// supervisor do not depend on the file being right, so they happen first and
// always; the PID kill is then a best-effort extra, not the load-bearing step.
func stopDaemonByPID() error {
	// Record the intent BEFORE anything dies. On Windows this is the only thing
	// that tells the launcher shim the exit was requested: taskkill /f gives a
	// stopped daemon exactly the same exit status as one an AV killed, so without
	// the marker the shim would relaunch it seconds after the user paused it —
	// the "I pause it and it turns itself back on" bug. The next daemon start
	// clears the marker, so it can never suppress a respawn after a LATER crash.
	agent.WriteStopIntent()
	// Then cut the supervisor. No-op off Windows (a real service manager already
	// owns this); on Windows it ends the scheduled task, taking the whole
	// wscript → cmd → unarr.exe tree with it regardless of what the state file
	// claims. See stopSupervisor.
	stopSupervisor()

	state, err := agent.LoadState()
	if err != nil {
		if errors.Is(err, agent.ErrDaemonNotRunning) {
			return err
		}
		return fmt.Errorf("read daemon state: %w", err)
	}
	if agent.IsProcessAlive(state.PID) {
		if err := killPID(state.PID); err != nil {
			return err
		}
	} else {
		// The state file outlived its daemon (a crash, or a respawn it had not
		// caught up with). Not an error: the supervisor is already stopped and the
		// stale file is about to be reaped, which is what the user asked for.
		fmt.Println("  Daemon was not running under that PID — cleaning up.")
	}
	// Reap whatever is left: the daemon the state file named, or the one the
	// supervisor just took down under a different PID.
	reapStoppedState()
	return nil
}

// reapStoppedState removes the state file once the process it names is gone.
//
// Unlike reapStateAfterExit it is not told which PID to expect, because after a
// supervisor stop we may not know it — the daemon that died can be a respawn the
// state file had not caught up with. It re-reads the file every tick and only
// removes it once THAT process is dead, so a daemon the user started again in
// the meantime keeps its state instead of having it deleted out from under it.
func reapStoppedState() {
	for i := 0; i < 20; i++ { // up to ~10s for the tree to go down
		st := agent.ReadState()
		if st == nil {
			return // already gone (clean shutdown reaped it)
		}
		if !agent.IsProcessAlive(st.PID) {
			agent.RemoveState()
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// reapStateAfterExit waits briefly for the signaled daemon to exit, then
// removes the orphaned state file it may have left behind. A daemon stopped in
// its first seconds of life (signal handlers not yet installed) — and EVERY
// Windows stop, where taskkill /f gives it no chance to clean up — dies
// without RemoveState. The stale "running" state + dead PID would then read as
// a CRASH to `unarr status` and to the unarr-desktop crash watcher, which
// would email a false crash report for a user-initiated stop.
func reapStateAfterExit(pid int) {
	for i := 0; i < 20; i++ { // up to ~10s for a graceful drain
		if !agent.IsProcessAlive(pid) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if st := agent.ReadState(); st != nil && st.PID == pid && !agent.IsProcessAlive(pid) {
		agent.RemoveState()
	}
}
