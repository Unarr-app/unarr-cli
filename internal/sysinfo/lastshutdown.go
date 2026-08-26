package sysinfo

import "time"

// lastShutdownFn is the per-platform implementation, overridable in tests.
var lastShutdownFn = platformLastShutdown

// LastShutdown returns the instant this host recorded its most recent clean
// shutdown, and whether it could be determined at all. A false ok means
// "unknown" — callers must treat it as "no verdict".
//
// It exists because BootTime alone cannot see a Windows FAST STARTUP shutdown.
// Fast Startup (on by default on Windows client SKUs) is a hybrid shutdown: the
// kernel session is HIBERNATED rather than torn down, and GetTickCount64 —
// biased interrupt time, which by design keeps counting across sleep and
// hibernation — carries straight over the power cycle. This is the same reason
// Task Manager shows uptimes of weeks on a machine that is shut down nightly.
// The apparent boot instant is then far older than the state file of a daemon
// the shutdown killed, so StateFromPreviousBoot sees a state file NEWER than
// the boot and calls a routine "shut the laptop down for the night" a crash.
//
// The two sources are complementary, which is why both are consulted:
//
//   - Fast Startup shutdown: the tick counter lies, the shutdown record is
//     written — this catches it.
//   - Power loss, BSOD, a yanked VM: nothing gets to write a shutdown record,
//     but the tick counter really does restart — BootTime catches it.
//
// MEASURED vs INFERRED, so the next reader knows which is which. Measured on
// Win11 in the VM harness (test/windows/smoke-boottime.ps1, check 6): the
// registry value exists, a real shutdown moves it forward, and this parse
// matches both the raw FILETIME and the shutdown Windows logged (event 6006)
// to within a second. Fast Startup being ON by default is measured too
// (HiberbootEnabled=1). The carry-over of GetTickCount64 ACROSS a hybrid
// shutdown is inferred, not measured — that harness runs on firmware with no
// hibernation support, so it cannot perform a hybrid shutdown at all
// (`powercfg /a`: "Fast Startup — Hibernation is not available"). It is the
// same documented behaviour that makes Task Manager report uptimes of weeks on
// a nightly-shut-down laptop. If it ever turns out not to hold, this signal
// degrades to "never fires", not to a wrong verdict.
func LastShutdown() (time.Time, bool) {
	return lastShutdownFn()
}
