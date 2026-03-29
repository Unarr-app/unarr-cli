//go:build !windows

package agent

import "syscall"

// IsProcessAlive checks if a process with the given PID is running.
// On Unix, sends signal 0 which checks existence without affecting the process.
func IsProcessAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
