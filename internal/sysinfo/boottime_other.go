//go:build !linux && !windows && !darwin

package sysinfo

import "time"

// platformBootTime has no source on this platform. "Unknown" is the honest
// answer and callers must degrade to their pre-existing behaviour rather than
// inventing a boot instant.
func platformBootTime() (time.Time, bool) { return time.Time{}, false }
