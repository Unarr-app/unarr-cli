//go:build !windows

package sysinfo

import "time"

// platformLastShutdown has no source outside Windows, and needs none: the
// hybrid-shutdown ambiguity LastShutdown exists to resolve is a Windows Fast
// Startup behaviour. Linux and darwin restart their boot clocks on every power
// cycle, so BootTime alone is authoritative there.
func platformLastShutdown() (time.Time, bool) { return time.Time{}, false }
