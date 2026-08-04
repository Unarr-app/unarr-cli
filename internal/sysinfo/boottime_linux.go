//go:build linux

package sysinfo

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// uptimeProcPath is overridable in tests.
var uptimeProcPath = "/proc/uptime"

// platformBootTime derives the boot instant from /proc/uptime, whose first
// field is seconds since boot measured on CLOCK_BOOTTIME — i.e. it keeps
// running across a suspend, which is exactly what this comparison needs (see
// BootTime). /proc/stat's btime would be an alternative, but it is recomputed
// from the current wall clock on every read, so an NTP step moves it.
func platformBootTime() (time.Time, bool) {
	data, err := os.ReadFile(uptimeProcPath)
	if err != nil {
		return time.Time{}, false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return time.Time{}, false
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || secs < 0 {
		return time.Time{}, false
	}
	return time.Now().Add(-time.Duration(secs * float64(time.Second))), true
}
