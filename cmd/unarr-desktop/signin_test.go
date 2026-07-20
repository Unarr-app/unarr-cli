package main

import "testing"

func TestSignInNeeded(t *testing.T) {
	authFail := controlFailure{title: "Agent: sign-in required", authRequired: true}
	otherFail := controlFailure{title: "Agent: start failed"}

	tests := []struct {
		name      string
		accountOK bool
		state     trayState
		fail      controlFailure
		want      bool
	}{
		{
			// The case that started this: the account row said "unavailable"
			// and the menu offered no way to do anything about it. Being told
			// something is wrong with your account without being offered the
			// fix is a dead end.
			name:  "no confirmed account, whatever the daemon is doing",
			state: stateRunning, want: true,
		},
		{
			name:  "not signed in while stopped",
			state: stateStopped, want: true,
		},
		{
			// A rejected credential during a control: every button would fail
			// the same way, so the one that helps has to be there.
			name: "credential rejected on a control", accountOK: true,
			state: stateFailed, fail: authFail, want: true,
		},
		{
			// A failure that is not about credentials must not send the user
			// to sign in again — it would change nothing.
			name: "a non-auth failure", accountOK: true,
			state: stateFailed, fail: otherFail, want: false,
		},
		{
			// With a working account and a running agent there is nothing to
			// fix, so the row stays out of the menu.
			name: "signed in and running", accountOK: true,
			state: stateRunning, want: false,
		},
		{
			name: "signed in but paused", accountOK: true,
			state: statePaused, want: false,
		},
		{
			// A crash is not an account problem.
			name: "signed in but crashed", accountOK: true,
			state: stateCrashed, want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := signInNeeded(tc.accountOK, tc.state, tc.fail, nil); got != tc.want {
				t.Errorf("signInNeeded(accountOK=%v, %v, %+v) = %v, want %v",
					tc.accountOK, tc.state, tc.fail, got, tc.want)
			}
		})
	}
}
