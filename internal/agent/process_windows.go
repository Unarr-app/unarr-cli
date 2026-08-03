//go:build windows

package agent

import (
	"syscall"
)

// processQueryLimitedInformation is PROCESS_QUERY_LIMITED_INFORMATION. It is the
// least privilege that still allows GetExitCodeProcess, and unlike
// PROCESS_QUERY_INFORMATION it is granted for processes running at a higher
// integrity level — so this works without elevating.
const processQueryLimitedInformation = 0x1000

// stillActive is STILL_ACTIVE (259): the exit code Windows reports for a process
// that has not exited yet.
const stillActive = 259

// IsProcessAlive reports whether a process with the given PID is running.
//
// It asks the OS. That is worth stating because the previous implementation did
// not: os.FindProcess is a no-op on Windows (it always succeeds), so the check
// fell back to reading the daemon's own state file and calling the process alive
// if its last heartbeat was under two minutes old. That heuristic is wrong in
// both directions, and both were measured on real Windows:
//
//   - A process killed a second ago still has a fresh heartbeat, so it read as
//     ALIVE. `unarr stop` therefore never reaped the state file it left behind,
//     and the leftover "status: running" + dead PID is exactly what the tray
//     reports — and emails — as a crash. A clean stop looked like a crash.
//   - A perfectly healthy daemon whose heartbeat went stale (an unreachable API,
//     a blocked sync loop) read as DEAD after two minutes, so the tray reported a
//     crash for an agent that was running the whole time.
//
// OpenProcess + GetExitCodeProcess answers the actual question. A PID that no
// longer exists cannot be opened; one that has exited reports its exit code
// rather than STILL_ACTIVE.
//
// PID reuse is the residual risk (Windows recycles PIDs): a long-dead PID that
// the OS has since handed to an unrelated process reads as alive. Callers pair
// this with the state file's own PID field, so the window is small and the
// failure is conservative — at worst a stale state file survives a little
// longer, which is what the reaper is for.
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false // no such process (or it is gone and fully reaped)
	}
	defer func() { _ = syscall.CloseHandle(h) }()

	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
