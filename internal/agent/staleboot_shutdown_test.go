package agent

import (
	"testing"
	"time"
)

// TestStateFromPreviousBootFastStartup is the crash report this signal was
// added for: a Windows box shut down at midnight, whose agent mailed a crash
// report for the shutdown.
//
// Fast Startup hibernates the kernel session instead of tearing it down, so
// GetTickCount64 carries over the power cycle and the apparent boot is days
// old — NEWER than nothing, older than every state file, so the boot check
// alone can never fire. The shutdown record is what makes the verdict.
func TestStateFromPreviousBootFastStartup(t *testing.T) {
	now := time.Now()
	killedAt := now.Add(-9 * time.Hour) // 00:02, when the user shut the box down

	// The tick counter never restarted: "boot" looks like three days ago.
	fakeBoot(t, now.Add(-72*time.Hour), true)
	fakeLastShutdown(t, killedAt.Add(20*time.Second), true)

	st := &DaemonState{
		Status:        "running",
		PID:           12820,
		StartedAt:     now.Add(-14 * time.Hour),
		LastHeartbeat: killedAt,
		LastAlive:     killedAt,
	}
	if !StateFromPreviousBoot(st) {
		t.Fatal("a state file older than the recorded shutdown is a power cycle, not a crash — this is the false crash report the fix exists to stop")
	}
}

// TestStateFromPreviousBootRealCrashAfterAShutdown is the other half: the same
// machine, the same stale-looking boot instant, but the daemon died while the
// current session was up. Anything written AFTER the last shutdown belongs to
// this session, so the report must still be sent.
func TestStateFromPreviousBootRealCrashAfterAShutdown(t *testing.T) {
	now := time.Now()
	fakeBoot(t, now.Add(-72*time.Hour), true)
	fakeLastShutdown(t, now.Add(-9*time.Hour), true) // last night's shutdown

	st := &DaemonState{
		Status:        "running",
		PID:           12820,
		StartedAt:     now.Add(-3 * time.Hour),
		LastHeartbeat: now.Add(-4 * time.Minute),
		LastAlive:     now.Add(-4 * time.Minute),
	}
	if StateFromPreviousBoot(st) {
		t.Fatal("a daemon that lived and died inside the current session is a crash worth reporting")
	}
}

// TestStateFromPreviousBootCrashSecondsAfterAReboot pins the "no positive
// slack" decision in writtenBeforeLastShutdown. A daemon that comes up right
// after a reboot and dies immediately is the MOST interesting crash there is —
// any slack applied the other way around would swallow exactly this case.
func TestStateFromPreviousBootCrashSecondsAfterAReboot(t *testing.T) {
	now := time.Now()
	shutdown := now.Add(-3 * time.Minute)
	fakeBoot(t, now.Add(-2*time.Minute), true) // honest boot clock this time
	fakeLastShutdown(t, shutdown, true)

	st := &DaemonState{
		Status:        "running",
		PID:           4242,
		StartedAt:     shutdown.Add(90 * time.Second), // came up after the reboot
		LastHeartbeat: shutdown.Add(100 * time.Second),
	}
	if StateFromPreviousBoot(st) {
		t.Fatal("a crash-at-startup right after a reboot must be reported, not filed as the reboot")
	}
}

// TestStateFromPreviousBootWithoutAShutdownRecord: a machine that has never
// been shut down cleanly (or a Linux/darwin host, where the source is a
// deliberate no-op) must fall straight back to the boot verdict.
func TestStateFromPreviousBootWithoutAShutdownRecord(t *testing.T) {
	now := time.Now()
	fakeBoot(t, now.Add(-30*time.Minute), true)
	fakeLastShutdown(t, time.Time{}, false)

	stale := &DaemonState{Status: "running", PID: 1, LastHeartbeat: now.Add(-6 * time.Hour)}
	if !StateFromPreviousBoot(stale) {
		t.Fatal("no shutdown record must leave the boot verdict working exactly as before")
	}
	fresh := &DaemonState{Status: "running", PID: 1, StartedAt: now.Add(-10 * time.Minute), LastHeartbeat: now.Add(-1 * time.Minute)}
	if StateFromPreviousBoot(fresh) {
		t.Fatal("no shutdown record must not invent a verdict either")
	}
}

// TestStateFromPreviousBootRejectsAnUntrustworthyShutdownRecord: a zero value
// is an uninitialised registry slot, and a record in the future means the wall
// clock moved. Either would make every state file look pre-shutdown and
// suppress every crash report on the host.
func TestStateFromPreviousBootRejectsAnUntrustworthyShutdownRecord(t *testing.T) {
	now := time.Now()
	fakeBoot(t, now.Add(-72*time.Hour), true) // boot check cannot fire on its own

	st := &DaemonState{Status: "running", PID: 1, StartedAt: now.Add(-2 * time.Hour), LastHeartbeat: now.Add(-2 * time.Minute)}

	fakeLastShutdown(t, time.Time{}, true)
	if StateFromPreviousBoot(st) {
		t.Fatal("a zero shutdown instant is not a shutdown: no verdict")
	}
	fakeLastShutdown(t, now.Add(2*time.Hour), true)
	if StateFromPreviousBoot(st) {
		t.Fatal("a shutdown recorded in the future is untrustworthy: no verdict")
	}
}
