//go:build !windows

package sysinfo

import "time"

// platformSessionLogonTime has no source outside Windows, and needs none: a
// systemd user unit or a launchd agent is stopped with SIGTERM when the session
// ends, and the daemon's signal path marks its state "shutting_down" before it
// drains — so a sign-out there never leaves the bare "running" this resolves.
func platformSessionLogonTime() (time.Time, bool) { return time.Time{}, false }
