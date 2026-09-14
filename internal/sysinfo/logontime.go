package sysinfo

import "time"

// logonTimeFn is the per-platform implementation, overridable in tests.
var logonTimeFn = platformSessionLogonTime

// SessionLogonTime returns the instant the user signed in to the session this
// process runs in, and whether it could be determined at all. A false ok means
// "unknown" — callers must treat it as "no verdict".
//
// It exists for the one way a Windows daemon dies without a trace that neither
// BootTime nor LastShutdown can see: a SIGN-OUT. Measured on the VM harness
// (test/windows/smoke-logoff-arm.ps1): a logoff kills the daemon the scheduled
// task's shim was running and leaves its state file saying "running" with a
// dead PID — no shutdown line, no shutdown record, and the boot instant does
// not move because nothing rebooted. The tray that starts at the next sign-in
// then reads a crash. A state file last written before this session began can
// only describe a daemon the previous session's end took down.
func SessionLogonTime() (time.Time, bool) {
	return logonTimeFn()
}
