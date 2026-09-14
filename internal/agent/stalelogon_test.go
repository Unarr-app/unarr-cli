package agent

import (
	"testing"
	"time"
)

func withLogonTime(t *testing.T, at time.Time, ok bool) {
	t.Helper()
	prev := logonTimeFn
	logonTimeFn = func() (time.Time, bool) { return at, ok }
	t.Cleanup(func() { logonTimeFn = prev })
}

// TestStateFromPreviousLogon is the sign-out case measured on the Windows
// harness: the daemon's last write lands just before the logoff, the next sign-in
// comes later, and nothing about the boot or a shutdown record moved.
func TestStateFromPreviousLogon(t *testing.T) {
	now := time.Now()
	logon := now.Add(-3 * time.Minute)

	cases := []struct {
		name    string
		written time.Time
		logonOK bool
		want    bool
	}{
		{"written before this sign-in: the sign-out killed it", logon.Add(-time.Second), true, true},
		{"written after this sign-in: a real crash in this session", logon.Add(time.Second), true, false},
		{"logon time unknown: no verdict", logon.Add(-time.Hour), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withLogonTime(t, logon, tc.logonOK)
			st := &DaemonState{Status: "running", PID: 4242, StartedAt: tc.written.Add(-time.Hour), LastAlive: tc.written}
			if got := StateFromPreviousLogon(st); got != tc.want {
				t.Errorf("StateFromPreviousLogon = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStateFromPreviousLogonDistrustsAFutureLogon: a logon instant after now means
// the clock moved, not that the user will sign in later.
func TestStateFromPreviousLogonDistrustsAFutureLogon(t *testing.T) {
	withLogonTime(t, time.Now().Add(time.Hour), true)
	st := &DaemonState{Status: "running", PID: 1, LastAlive: time.Now().Add(-time.Minute)}
	if StateFromPreviousLogon(st) {
		t.Error("a logon in the future swallowed a state file")
	}
	if StateFromPreviousLogon(nil) || StateFromPreviousLogon(&DaemonState{PID: 1}) {
		t.Error("no state, or a state with no timestamps, must give no verdict")
	}
}
