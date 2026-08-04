//go:build darwin

package sysinfo

import (
	"time"

	"golang.org/x/sys/unix"
)

// platformBootTime reads kern.boottime, which the darwin kernel keeps as an
// absolute wall-clock timestamp of the boot — no uptime arithmetic, and no
// question about whether sleep is counted (the value does not tick at all).
func platformBootTime() (time.Time, bool) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil || tv == nil || tv.Sec <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(tv.Sec), int64(tv.Usec)*1000), true
}
