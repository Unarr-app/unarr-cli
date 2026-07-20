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

func TestAStaleBlockNeverStrandsTheUser(t *testing.T) {
	// A record left behind by a daemon that has since stopped is stale by
	// definition. Letting it win disabled every control — including Resume, the
	// ONLY one that starts the agent — so the user's way out was deleting a file
	// by hand. Starting it is exactly right: it either re-registers and clears
	// the record, or parks again and re-states the problem with fresh facts.
	got := displayState(agentStatus{}, false, false, true)
	if got == stateBlocked {
		t.Fatal("a block with no live daemon still wins, leaving no control that can start the agent")
	}
	if got != stateStopped {
		t.Errorf("displayState(stopped, stale block) = %v, want %v", got, stateStopped)
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

func TestVPNKillSwitchIsNotAHealthyAgent(t *testing.T) {
	// The kill-switch working as asked is still a total outage for downloads.
	// The tray read only "is the PID alive", so it showed a green running agent
	// while no torrent could move — sending the user to look for a broken
	// download instead of a dead tunnel.
	got := displayState(agentStatus{running: true, vpnBlocking: true}, false, false, false)
	if got == stateRunning || got == stateDownloading {
		t.Fatalf("displayState(vpn blocking) = %v — downloads are dead but the icon says fine", got)
	}
	if got != stateVPNBlocked {
		t.Errorf("displayState(vpn blocking) = %v, want %v", got, stateVPNBlocked)
	}
}

func TestACredentialBlockOutranksTheVPNKillSwitch(t *testing.T) {
	// Both stop downloads, but only one needs the user. If the credential is
	// rejected, reconnecting the VPN changes nothing.
	got := displayState(agentStatus{running: true, vpnBlocking: true}, false, false, true)
	if got != stateBlocked {
		t.Errorf("displayState(blocked + vpn) = %v, want %v", got, stateBlocked)
	}
}

func TestVPNBlockedLooksDifferentFromBothRunningAndBroken(t *testing.T) {
	icons := buildStateIcons(trayIcon)
	vpn, ok := icons[stateVPNBlocked]
	if !ok || len(vpn) == 0 {
		t.Fatal("no icon for the VPN-blocked state")
	}
	if string(vpn) == string(icons[stateRunning]) {
		t.Error("wears the running icon: the outage is invisible at a glance")
	}
	if string(vpn) == string(icons[stateCrashed]) {
		t.Error("wears the crashed icon: nothing has failed, the kill-switch is doing its job")
	}
}

func TestOutdatedAgentAsksForAnUpdateNotASignIn(t *testing.T) {
	b := &agent.Blocked{Reason: agent.BlockVersion}
	if title := blockedTitle(b); !strings.Contains(strings.ToLower(title), "update") {
		t.Errorf("title = %q, want it to name the update", title)
	}
	if blockNeedsSignIn(b) {
		t.Error("offered sign-in for an outdated agent: signing in would succeed and change nothing")
	}
}
