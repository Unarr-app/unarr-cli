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

// stopDaemonByPID reads the state file and sends a graceful stop to the daemon PID.
// Used as fallback on platforms without a service manager (and as Windows implementation).
func stopDaemonByPID() error {
	state, err := agent.LoadState()
	if err != nil {
		if errors.Is(err, agent.ErrDaemonNotRunning) {
			// Nothing to signal, but "stop" was still asked for, and on Windows the
			// supervisor may be about to bring the daemon back. Record the intent so
			// it does not.
			agent.WriteStopIntent()
			return err
		}
		return fmt.Errorf("read daemon state: %w", err)
	}
	if !agent.IsProcessAlive(state.PID) {
		// Daemon already dead (crashed or fatal-exited) but left its state file
		// behind. "Stopping" it should clean that up and succeed — this is the
		// tray's Pause-after-crash path, not an error.
		//
		// The intent still has to be recorded: the process is gone, but the
		// scheduled task that owns it is not, and a pending restart would undo the
		// user's pause moments after they asked for it. Verified on real Windows —
		// this branch is exactly the one a tray Pause hits after a crash.
		agent.WriteStopIntent()
		agent.RemoveState()
		fmt.Println("  Daemon was not running — cleaned up stale state file.")
		return nil
	}
	// Record the intent BEFORE the kill. On Windows this is the only thing that
	// tells the launcher shim (and through it the scheduled task) that the exit
	// about to happen was requested: taskkill /f gives a stopped daemon exactly
	// the same exit status as one an AV killed, so without the marker the task
	// would respawn the daemon a minute after the user paused it — the
	// "I pause it and it turns itself back on" bug. The next daemon start clears
	// the marker, so it can never suppress the respawn after a LATER crash.
	agent.WriteStopIntent()
	if err := killPID(state.PID); err != nil {
		return err
	}
	reapStateAfterExit(state.PID)
	return nil
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
