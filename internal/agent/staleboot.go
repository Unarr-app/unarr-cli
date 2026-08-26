package agent

import (
	"time"

	"github.com/Unarr-app/unarr-cli/internal/sysinfo"
)

// bootTimeFn / lastShutdownFn are overridable in tests.
var (
	bootTimeFn     = sysinfo.BootTime
	lastShutdownFn = sysinfo.LastShutdown
)

// preBootSlack is how far a state file may predate the boot instant before it
// is judged to belong to the previous boot.
//
// It absorbs measurement jitter (uptime is read as a float, the wall clock is
// sampled a moment later), NOT clock drift — an hour of slack would be a licence
// to misclassify. The direction of the remaining error matters more than its
// size: too little slack merely leaves today's behaviour (a report we already
// send), while too much would swallow a real crash. So it is small on purpose.
const preBootSlack = 2 * time.Minute

// StateFromPreviousBoot reports whether this state file was written before the
// host booted — i.e. the daemon did not die, the MACHINE went down under it.
//
// This is the difference between a crash report worth sending and noise. A
// Windows box that installs updates in its 02:00–05:00 maintenance window
// reboots with the daemon up; the daemon gets at most a CTRL_SHUTDOWN_EVENT and
// ~5s, while its shutdown path drains downloads for up to 30s (see
// cmd.runDaemon), so it is killed before it can seal and remove its state file.
// What survives is a state file saying "running" whose PID is gone — byte for
// byte what a panic leaves behind. The tray then mails a crash report for a
// reboot the user asked for.
//
// It also settles a second, quieter failure: PID REUSE. After a reboot the
// recorded PID can belong to some unrelated process, and IsProcessAlive says
// yes — so the tray renders a healthy green agent that is not running at all.
// A state file older than the boot is stale REGARDLESS of what its PID says,
// which is why callers must consult this BEFORE the liveness check.
//
// TWO independent signals, either of which is enough (see the helpers below):
// a state file older than the boot instant, or older than the last recorded
// shutdown. Neither covers the other — Windows Fast Startup breaks the first,
// a power cut breaks the second — and both err toward "no verdict".
//
// Unknown boot time (an unsupported platform, an unreadable /proc) returns
// false: no verdict, keep the previous behaviour. Same for a state file with no
// timestamps at all — being wrong here costs a lost crash report.
func StateFromPreviousBoot(st *DaemonState) bool {
	if st == nil {
		return false
	}
	written := stateWrittenAt(st)
	if written.IsZero() {
		return false
	}
	if writtenBeforeBoot(written) {
		return true
	}
	return writtenBeforeLastShutdown(written)
}

// writtenBeforeBoot is the original verdict: the state file predates the
// instant this host booted, so the machine came down under the daemon.
func writtenBeforeBoot(written time.Time) bool {
	boot, ok := bootTimeFn()
	if !ok || boot.IsZero() {
		return false
	}
	// A boot instant in the future means the arithmetic behind it is not to be
	// trusted (a wall clock stepped backwards after boot, a suspended VM
	// restored with a stale RTC). Refuse to rule rather than rule wrongly.
	if boot.After(time.Now()) {
		return false
	}
	return written.Before(boot.Add(-preBootSlack))
}

// writtenBeforeLastShutdown is the second verdict, and the one that settles
// Windows FAST STARTUP. A hybrid shutdown hibernates the kernel session instead
// of tearing it down, so GetTickCount64 — and therefore writtenBeforeBoot —
// carries over the power cycle and reports a boot from days ago, NEWER than
// nothing and older than every state file. The shutdown record does not carry
// over: anything written before it belongs to a session that has since ended.
//
// NO SLACK, in either direction, unlike the boot comparison:
//
//   - none needed the pre-shutdown way: both instants are absolute wall clock,
//     with no uptime arithmetic between them to jitter;
//   - none WANTED the post-shutdown way, because the daemon writes its state
//     continuously up to the moment it is killed, so a real shutdown leaves
//     `written` a hair BEFORE the record, and any positive slack would start
//     swallowing genuine crashes that happen seconds into the next boot — the
//     crash-at-startup case, which is exactly the one worth reporting.
func writtenBeforeLastShutdown(written time.Time) bool {
	down, ok := lastShutdownFn()
	if !ok || down.IsZero() {
		return false
	}
	// Same distrust as above: a shutdown recorded in the future means the clock
	// moved, not that the machine went down after now.
	if down.After(time.Now()) {
		return false
	}
	return written.Before(down)
}

// stateWrittenAt is the most recent instant this state file can be shown to
// have been current. The heartbeat is the live one; StartedAt covers the window
// between a daemon's first write and its first heartbeat, and is what keeps a
// daemon that has just come up from being read as pre-boot.
//
// Taking the NEWER of the two is load-bearing beyond that: LastHeartbeat only
// advances on a SUCCESSFUL sync (see Daemon.OnSyncSuccess), so a daemon that
// has been up for hours with no network has an hours-old heartbeat. Judging by
// the heartbeat alone would declare that live daemon's state pre-boot and reap
// it. StartedAt cannot go stale that way.
func stateWrittenAt(st *DaemonState) time.Time {
	written := LastAliveAt(st)
	if st.StartedAt.After(written) {
		written = st.StartedAt
	}
	return written
}

// LastAliveAt is the most recent moment this daemon is known to have been
// running: its liveness stamp, or the last successful sync for a state file
// written before LastAlive existed. Zero when neither is set.
//
// The two fields answer different questions and the difference is load-bearing.
// LastHeartbeat advances only when the SERVER answered, so it freezes on a box
// with no network while the daemon keeps working; LastAlive advances on every
// sync attempt. Reading the heartbeat for liveness is what made `unarr status`
// call a downloading agent "not running".
func LastAliveAt(st *DaemonState) time.Time {
	if st == nil {
		return time.Time{}
	}
	if st.LastAlive.After(st.LastHeartbeat) {
		return st.LastAlive
	}
	return st.LastHeartbeat
}
