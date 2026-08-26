// Package sysinfo answers questions about the HOST the agent runs on, as
// opposed to the agent's own state: when did this machine boot, and when did it
// last shut down?
//
// It exists because "the daemon is gone and its state file still says running"
// has two very different causes that look identical on disk:
//
//   - the daemon died (a panic, an AV kill, an OOM) — a crash worth reporting;
//   - the HOST went down under it (restart, shutdown, a Windows Update reboot
//     during the 02:00–05:00 maintenance window) — not a crash at all.
//
// A state file written BEFORE the current boot can only be the second case, so
// the boot instant is what tells them apart — except where the boot instant
// itself survives a power cycle (Windows Fast Startup), which is what
// LastShutdown is for. See agent.StateFromPreviousBoot.
package sysinfo

import "time"

// bootTimeFn is the per-platform implementation, overridable in tests.
var bootTimeFn = platformBootTime

// BootTime returns the wall-clock instant this host booted, and whether it
// could be determined at all. A false ok means "unknown" — callers must treat
// that as "no verdict", never as "booted at the zero time".
//
// SUSPENSION IS COUNTED, deliberately, on every platform: the underlying source
// is boot-based (Windows GetTickCount64, Linux /proc/uptime's CLOCK_BOOTTIME,
// the darwin kern.boottime timestamp), NOT the "unbiased" variants that stop
// ticking while the machine sleeps. A laptop that slept eight hours overnight
// and then really did crash must still be judged against its REAL boot instant;
// an unbiased clock would place the apparent boot after the crash and quietly
// reclassify it as a reboot — losing the very report this all exists for.
func BootTime() (time.Time, bool) {
	return bootTimeFn()
}
