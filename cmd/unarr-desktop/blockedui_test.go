package main

import (
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

func TestBlockedOutranksRunning(t *testing.T) {
	// The bug this exists for: a daemon whose every request the server rejects
	// still has a live PID, and the tray's health check was "is the PID alive".
	// So a totally useless agent rendered as a healthy green "running" — the
	// worst state in the product, because every indicator said it was fine.
	got := displayState(agentStatus{running: true, pid: 42}, false, false, true)
	if got != stateBlocked {
		t.Errorf("displayState(running, blocked) = %v, want %v", got, stateBlocked)
	}
}

func TestBlockedOutranksAnActiveDownloadCount(t *testing.T) {
	// A stale task count from before the block must not paint it as busy.
	got := displayState(agentStatus{running: true, tasks: 3}, false, false, true)
	if got != stateBlocked {
		t.Errorf("displayState(downloading, blocked) = %v, want %v", got, stateBlocked)
	}
}

func TestNotBlockedKeepsTheOldBehaviour(t *testing.T) {
	if got := displayState(agentStatus{running: true}, false, false, false); got != stateRunning {
		t.Errorf("displayState(running) = %v, want %v", got, stateRunning)
	}
}

func TestBlockedTitleNamesTheProblem(t *testing.T) {
	reasons := []agent.BlockReason{
		agent.BlockSignIn, agent.BlockRevoked, agent.BlockPlan, agent.BlockConflict,
	}
	seen := map[string]bool{}
	for _, r := range reasons {
		title := blockedTitle(&agent.Blocked{Reason: r})
		if title == "" {
			t.Fatalf("%s has no status row", r)
		}
		if strings.Contains(title, string(r)) {
			t.Errorf("%s renders the machine code: %q", r, title)
		}
		if seen[title] {
			t.Errorf("%s reuses the title %q — distinct problems need distinct rows", r, title)
		}
		seen[title] = true
	}
	// An unknown reason from a newer daemon must still say something actionable
	// rather than fall back to a bare "stopped" that hides the block.
	if got := blockedTitle(&agent.Blocked{Reason: "something_new"}); !strings.Contains(got, "action") {
		t.Errorf("unknown reason = %q, want it to still ask the user to act", got)
	}
}

func TestSignInIsOfferedOnlyWhenItWouldHelp(t *testing.T) {
	tests := []struct {
		reason agent.BlockReason
		want   bool
	}{
		{agent.BlockSignIn, true},
		{agent.BlockRevoked, true},
		{agent.BlockConflict, true},
		// Signing in again would succeed and change nothing: the account is out
		// of machine slots. Offering it would be a dead end wearing the costume
		// of a fix.
		{agent.BlockPlan, false},
	}
	for _, tc := range tests {
		t.Run(string(tc.reason), func(t *testing.T) {
			b := &agent.Blocked{Reason: tc.reason}
			if got := blockNeedsSignIn(b); got != tc.want {
				t.Errorf("blockNeedsSignIn(%s) = %v, want %v", tc.reason, got, tc.want)
			}
			// signInNeeded must defer to the daemon's diagnosis, not to the
			// tray's own account guesswork, once a block is on disk.
			if got := signInNeeded(true, stateBlocked, controlFailure{}, b); got != tc.want {
				t.Errorf("signInNeeded(blocked=%s) = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}

func TestBlockedOverridesTheAccountGuess(t *testing.T) {
	// Without a block, "no confirmed account" offers sign-in. With a plan block
	// it must not: the daemon talked to the server and knows better.
	if !signInNeeded(false, stateRunning, controlFailure{}, nil) {
		t.Error("no account and no block: sign-in should be offered")
	}
	if signInNeeded(false, stateBlocked, controlFailure{}, &agent.Blocked{Reason: agent.BlockPlan}) {
		t.Error("a plan block must not send the user to sign in, even with no account fetched")
	}
}

func TestBlockedTooltipSaysWhatIsWrong(t *testing.T) {
	// This hover is the only thing the user gets without opening the menu.
	b := &agent.Blocked{Reason: agent.BlockSignIn}
	got := trayTooltip(stateBlocked, agentStatus{running: true}, b)
	if strings.Contains(got, "blocked") {
		t.Errorf("tooltip = %q, want the problem rather than the jargon", got)
	}
	if !strings.Contains(got, "sign-in") {
		t.Errorf("tooltip = %q, want it to name the sign-in problem", got)
	}
}

func TestBlockedTitleSurvivesANilBlock(t *testing.T) {
	// Defensive: the state and the record are read separately, so a block that
	// clears between the two must not panic the tray.
	if got := blockedTitle(nil); got == "" {
		t.Error("blockedTitle(nil) is empty")
	}
	if blockNeedsSignIn(nil) {
		t.Error("blockNeedsSignIn(nil) = true")
	}
}

func TestBlockedIconIsDistinctFromRunning(t *testing.T) {
	icons := buildStateIcons(trayIcon)
	blocked, ok := icons[stateBlocked]
	if !ok || len(blocked) == 0 {
		t.Fatal("no icon for the blocked state")
	}
	if string(blocked) == string(icons[stateRunning]) {
		t.Error("blocked wears the running icon — the badge is the only at-a-glance signal")
	}
}
