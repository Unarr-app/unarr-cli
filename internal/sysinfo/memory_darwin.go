//go:build darwin

package sysinfo

import "golang.org/x/sys/unix"

// platformTotalMemory is the installed RAM (hw.memsize).
func platformTotalMemory() (uint64, bool) {
	v, err := unix.SysctlUint64("hw.memsize")
	return v, err == nil && v > 0
}
