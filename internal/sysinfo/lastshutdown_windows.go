//go:build windows

package sysinfo

import (
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// shutdownTimeKey / shutdownTimeValue hold a FILETIME (8 bytes, little-endian,
// 100ns ticks since 1601-01-01 UTC) that the session manager stamps as the
// machine goes down — including a Fast Startup hybrid shutdown, which is the
// case this whole path exists for. Absent on a machine that has never been
// shut down cleanly since install, and after a power loss it simply stays at
// the previous value: both degrade to "unknown"/"older than the state file",
// which is the safe direction (a crash report is still sent).
const (
	shutdownTimeKey   = `SYSTEM\CurrentControlSet\Control\Windows`
	shutdownTimeValue = "ShutdownTime"
)

func platformLastShutdown() (time.Time, bool) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, shutdownTimeKey, registry.QUERY_VALUE)
	if err != nil {
		return time.Time{}, false // key missing, or no read access from this token
	}
	defer k.Close()

	buf, _, err := k.GetBinaryValue(shutdownTimeValue)
	if err != nil || len(buf) < 8 {
		return time.Time{}, false
	}
	ft := windows.Filetime{
		LowDateTime: uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24,
		HighDateTime: uint32(buf[4]) | uint32(buf[5])<<8 | uint32(buf[6])<<16 |
			uint32(buf[7])<<24,
	}
	// A zeroed FILETIME is not a shutdown at the start of the 17th century; it
	// is an uninitialised value. Refuse to rule on it.
	if ft.LowDateTime == 0 && ft.HighDateTime == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, ft.Nanoseconds()).UTC(), true
}
