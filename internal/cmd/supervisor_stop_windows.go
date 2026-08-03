//go:build windows

package cmd

import (
	"os/exec"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// stopSupervisor ends the scheduled task that owns the daemon on Windows.
//
// It exists because stopping by PID alone cannot be trusted to stop the agent.
// `unarr stop` finds its target in the daemon state file, which is written
// during registration — seconds into startup — and SURVIVES a crash. So in the
// window between the launcher shim relaunching a crashed daemon and that new
// daemon registering, the state file still names the previous, dead PID: stop
// then takes the "daemon already dead" branch, kills nothing, and the live
// daemon keeps running. Measured on real Windows, and now a recurring window
// rather than a rare one, because the shim relaunches on every crash.
//
// Ending the task cuts the whole tree the task owns — wscript.exe running the
// shim, the cmd.exe wrapper, and unarr.exe underneath it — without consulting
// the state file at all. That is the property that makes stop reliable: it does
// not depend on our bookkeeping being accurate.
//
// Best-effort by design. A foreground `unarr start` has no task, an agent that
// was never installed as a service has no task, and `schtasks` reports an error
// for both — none of which is a failure of "stop". The PID path still runs
// afterwards and covers exactly those cases.
//
// Pairs with the stop-intent marker: the marker is written first, so even if a
// relaunch is already in flight it sees "stopped on purpose" and stands down.
func stopSupervisor() {
	cmd := exec.Command("schtasks", "/end", "/tn", "unarr")
	winproc.HideWindow(cmd)
	_ = cmd.Run() // no task / not running / not installed — all fine
}
