//go:build linux

package sysinfo

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// cgroupMemoryLimitPaths are the container memory limits, cgroup v2 then v1.
// Overridable in tests.
var cgroupMemoryLimitPaths = []string{
	"/sys/fs/cgroup/memory.max",
	"/sys/fs/cgroup/memory/memory.limit_in_bytes",
}

// platformTotalMemory is sysinfo(2)'s total RAM, capped by the cgroup limit. A
// NAS app runs in a container more often than not, where the host's RAM says
// nothing about what the process may use before the OOM killer steps in.
func platformTotalMemory() (uint64, bool) {
	var si unix.Sysinfo_t
	if err := unix.Sysinfo(&si); err != nil {
		return 0, false
	}
	total := uint64(si.Totalram) * uint64(si.Unit)
	if limit, ok := cgroupMemoryLimit(); ok && (total == 0 || limit < total) {
		total = limit
	}
	return total, total > 0
}

// cgroupMemoryLimit returns the first numeric limit found. "max" (v2) means no
// limit; v1's "unlimited" is a huge number, which the caller's min discards.
func cgroupMemoryLimit() (uint64, bool) {
	for _, path := range cgroupMemoryLimitPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		if err == nil && v > 0 {
			return v, true
		}
	}
	return 0, false
}
