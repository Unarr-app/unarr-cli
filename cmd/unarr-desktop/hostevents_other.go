//go:build !windows

package main

import "time"

// hostEventsSection adds nothing outside Windows. A systemd or launchd daemon's
// death is recorded by its own service manager, whose log `unarr logs` already
// reads (journald), and an OOM kill lands in the kernel log with the unit name.
func hostEventsSection(time.Time) string { return "" }
