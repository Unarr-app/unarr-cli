//go:build windows

package agent

import (
	"os"
	"time"
)

// IsProcessAlive checks if a process with the given PID is running.
// On Windows, os.FindProcess + a zero-timeout wait is used since
// signal 0 is not supported.
func IsProcessAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Windows, FindProcess always succeeds. Use the state file's
	// last heartbeat as a heuristic: if it's recent, the process is alive.
	state := ReadState()
	if state == nil || state.PID != pid {
		return false
	}
	return time.Since(state.LastHeartbeat) < 2*time.Minute
}
