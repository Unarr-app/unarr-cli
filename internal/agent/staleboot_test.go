package agent

import (
	"testing"
	"time"
)

// fakeBoot pins the boot instant for one test.
func fakeBoot(t *testing.T, at time.Time, ok bool) {
	t.Helper()
	restore := bootTimeFn
	t.Cleanup(func() { bootTimeFn = restore })
	bootTimeFn = func() (time.Time, bool) { return at, ok }
}

func TestStateFromPreviousBoot(t *testing.T) {
	now := time.Now()
	boot := now.Add(-30 * time.Minute)

	cases := []struct {
		name  string
		state *DaemonState
		want  bool
		why   string
	}{
		{
			name:  "written before the boot is a reboot, not a crash",
			state: &DaemonState{Status: "running", PID: 12272, StartedAt: now.Add(-8 * time.Hour), LastHeartbeat: now.Add(-35 * time.Minute)},
			want:  true,
			why:   "the machine went down under a daemon that had been up for hours",
		},
		{
			name:  "written after the boot is a real death",
			state: &DaemonState{Status: "running", PID: 12272, StartedAt: now.Add(-20 * time.Minute), LastHeartbeat: now.Add(-1 * time.Minute)},
			want:  false,
			why:   "this daemon lived and died inside the current boot — report it",
		},
		{
			name:  "inside the slack window is not a verdict",
			state: &DaemonState{Status: "running", PID: 1, StartedAt: boot.Add(-30 * time.Second), LastHeartbeat: boot.Add(-30 * time.Second)},
			want:  false,
			why:   "measurement jitter must never cost a real crash report",
		},
		{
			name:  "exactly at the slack boundary is not a verdict",
			state: &DaemonState{Status: "running", PID: 1, StartedAt: boot.Add(-preBootSlack), LastHeartbeat: boot.Add(-preBootSlack)},
			want:  false,
			why:   "the comparison is strictly-before, so the boundary itself stays a crash",
		},
		{
			name:  "one nanosecond past the boundary flips it",
			state: &DaemonState{Status: "running", PID: 1, StartedAt: boot.Add(-preBootSlack - time.Nanosecond), LastHeartbeat: boot.Add(-preBootSlack - time.Nanosecond)},
			want:  true,
			why:   "the boundary must be exact, not approximate",
		},
		{
			name:  "StartedAt alone carries a daemon that has not heartbeat yet",
			state: &DaemonState{Status: "running", PID: 1, StartedAt: now.Add(-2 * time.Minute)},
			want:  false,
			why:   "a daemon that just came up post-boot has no heartbeat, and is not stale",
		},
		{
			name:  "a live daemon with a stale heartbeat is judged by StartedAt",
			state: &DaemonState{Status: "running", PID: 1, StartedAt: now.Add(-25 * time.Minute), LastHeartbeat: now.Add(-9 * time.Hour)},
			want:  false,
			why:   "LastHeartbeat only advances on a SUCCESSFUL sync, so an offline daemon has an ancient one — reaping it would kill the tray's view of a running agent",
		},
		{
			name:  "heartbeat alone, from before the boot",
			state: &DaemonState{Status: "running", PID: 1, LastHeartbeat: now.Add(-6 * time.Hour)},
			want:  true,
			why:   "a state file with no StartedAt still dates itself by its heartbeat",
		},
		{
			name:  "a non-running status is judged the same way",
			state: &DaemonState{Status: "shutting_down", PID: 1, StartedAt: now.Add(-8 * time.Hour), LastHeartbeat: now.Add(-7 * time.Hour)},
			want:  true,
			why:   "staleness is about WHEN it was written, not what it claims; the caller decides what to do with each status",
		},
		{
			name:  "no timestamps at all yields no verdict",
			state: &DaemonState{Status: "running", PID: 1},
			want:  false,
			why:   "an undatable state file must not be silently reaped",
		},
		{name: "nil state", state: nil, want: false, why: "nothing to judge"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeBoot(t, boot, true)
			if got := StateFromPreviousBoot(tc.state); got != tc.want {
				t.Fatalf("StateFromPreviousBoot() = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestStateFromPreviousBootWithoutASource: an unsupported platform (or an
// unreadable /proc) must leave today's behaviour exactly as it was. Never
// "unknown boot ⇒ everything is stale" — that would suppress every crash
// report on that platform.
func TestStateFromPreviousBootWithoutASource(t *testing.T) {
	fakeBoot(t, time.Time{}, false)
	st := &DaemonState{Status: "running", PID: 1, LastHeartbeat: time.Now().Add(-72 * time.Hour)}
	if StateFromPreviousBoot(st) {
		t.Fatal("no boot-time source must mean no verdict, however old the state file is")
	}
}

// TestStateFromPreviousBootRejectsAZeroBoot guards the other half of that
// contract: a source that reports ok=true but hands back the zero time (a
// future platform stub getting it wrong) must not make every state file look
// pre-boot.
func TestStateFromPreviousBootRejectsAZeroBoot(t *testing.T) {
	fakeBoot(t, time.Time{}, true)
	st := &DaemonState{Status: "running", PID: 1, LastHeartbeat: time.Now().Add(-5 * time.Minute)}
	if StateFromPreviousBoot(st) {
		t.Fatal("a zero boot instant is not a boot instant: no verdict")
	}
}

// TestStateFromPreviousBootRefusesAFutureBoot: a wall clock that stepped
// backwards after boot (a VM restored from a snapshot, a bad NTP sync) makes
// the arithmetic produce a boot in the future. Every state file would then look
// pre-boot and every crash would be dismissed as a reboot, so this refuses to
// rule at all.
func TestStateFromPreviousBootRefusesAFutureBoot(t *testing.T) {
	fakeBoot(t, time.Now().Add(2*time.Hour), true)
	st := &DaemonState{Status: "running", PID: 1, LastHeartbeat: time.Now().Add(-5 * time.Minute)}
	if StateFromPreviousBoot(st) {
		t.Fatal("a boot instant in the future is untrustworthy: no verdict")
	}
}

// TestStateFromPreviousBootSurvivesALongSleep is the laptop case that decided
// which clock the Windows implementation reads. The box booted on Monday, slept
// eight hours overnight, and the daemon then genuinely crashed on Tuesday. As
// long as the boot instant counts the sleep (GetTickCount64, CLOCK_BOOTTIME),
// the crash stays a crash. An "unbiased" clock would place the apparent boot
// after the crash and swallow the report.
func TestStateFromPreviousBootSurvivesALongSleep(t *testing.T) {
	now := time.Now()
	realBoot := now.Add(-24 * time.Hour) // includes the 8h asleep
	fakeBoot(t, realBoot, true)

	crashed := &DaemonState{Status: "running", PID: 12272, StartedAt: now.Add(-23 * time.Hour), LastHeartbeat: now.Add(-3 * time.Minute)}
	if StateFromPreviousBoot(crashed) {
		t.Fatal("a real crash after a long sleep must still be reported")
	}

	// And the counterpart: the same box moments after a reboot. The state file
	// still carries the heartbeat from before the machine went down, which now
	// predates the boot, so it is correctly dismissed.
	fakeBoot(t, now.Add(-30*time.Second), true)
	if !StateFromPreviousBoot(crashed) {
		t.Fatal("a state file older than the current boot is a reboot leftover")
	}
}
