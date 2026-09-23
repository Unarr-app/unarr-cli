package cmd

import (
	"errors"
	"fmt"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/doctor"
)

// daemonFirstSyncGrace is how long a freshly started daemon may go without a
// single sync attempt before the health check calls it stuck. The sync loop
// makes its first attempt right after startup and then every few seconds, so
// minutes of silence mean it never started — not that it is slow.
const daemonFirstSyncGrace = 5 * time.Minute

// doctorDaemonSpec reports whether the daemon this machine is supposed to be
// running is alive AND working. It reads the state file and checks the PID —
// no network.
//
// It is the reason `--quick` exists. A container whose daemon has died keeps
// reporting "running" to Docker forever without this: the entrypoint process
// is still up, and nothing else looks at whether the thing it supervises is.
//
// A daemon that was never installed is a PASS, not a failure: `unarr` is a CLI
// too, and someone running one-off commands has no daemon by design. Only a
// registered-then-vanished daemon is a fault, and that is what a stale state
// file with a dead PID means.
func doctorDaemonSpec() doctor.Spec {
	return doctor.Spec{
		Group: "Daemon",
		Name:  "Daemon process",
		Quick: true,
		Fn: func() (string, error) {
			return daemonHealth(agent.ReadState(), time.Now(), isDaemonAlive)
		},
	}
}

// daemonHealth is the pure decision behind doctorDaemonSpec.
//
// A live PID is not enough. On 2026-09-23 a NAS daemon sat for a day with its
// main goroutine blocked in the startup resume, before the sync loop ever ran:
// the process was up, the web showed the agent offline, and this check passed —
// the state file register() writes carries no sync stamp yet, and the
// staleness rule in isDaemonAlive only judges a stamp that exists. So a daemon
// that has been up past daemonFirstSyncGrace without one attempt is stuck.
func daemonHealth(state *agent.DaemonState, now time.Time, alive func(*agent.DaemonState) bool) (string, error) {
	if state == nil {
		return "not running (no daemon installed on this machine)", nil
	}
	if !alive(state) {
		if last := agent.LastAliveAt(state); !last.IsZero() && now.Sub(last) > staleStateAfter {
			return fmt.Sprintf("pid %d stopped syncing %s ago (hung, or the process is gone)",
					state.PID, now.Sub(last).Round(time.Second)),
				errors.New("daemon not syncing")
		}
		return fmt.Sprintf("state file says pid %d, but that process is gone", state.PID),
			errors.New("daemon died")
	}
	up := now.Sub(state.StartedAt).Round(time.Second)
	if agent.LastAliveAt(state).IsZero() && !state.StartedAt.IsZero() && up > daemonFirstSyncGrace {
		return fmt.Sprintf("pid %d up %s but its sync loop never ran (stuck during startup)", state.PID, up),
			errors.New("daemon stuck before first sync")
	}
	return fmt.Sprintf("running (pid %d, up %s)", state.PID, up), nil
}
