package main

// How a terminal daemon failure is presented in the tray.
//
// The daemon records WHY it cannot work (a rejected credential, an exhausted
// plan) along with the one thing the user can do about it. The tray's job is to
// carry that faithfully: name the problem in the status row, and put the action
// that resolves it in the menu. Nothing here re-derives the diagnosis — the
// tray never sees the daemon's exit code or its stderr, which is precisely how
// this class of failure used to render as a healthy green agent.

import "github.com/Unarr-app/unarr-cli/internal/agent"

// blockedTitle is the status row for a blocked agent. It names the problem
// rather than the symptom: "Agent: running" was technically true while nothing
// worked, which is worse than saying nothing at all.
func blockedTitle(b *agent.Blocked) string {
	if b == nil {
		return "Agent: stopped"
	}
	switch b.Reason {
	case agent.BlockSignIn:
		return "Agent: sign-in required"
	case agent.BlockRevoked:
		return "Agent: disconnected from your account"
	case agent.BlockPlan:
		return "Agent: plan limit reached"
	case agent.BlockConflict:
		return "Agent: identity conflict"
	case agent.BlockVersion:
		return "Agent: update required"
	default:
		return "Agent: stopped — action needed"
	}
}

// blockNeedsSignIn reports whether re-authorizing this machine is what resolves
// the block. A plan limit is the exception: signing in again would succeed and
// change nothing, so sending the user there would be a dead end wearing the
// costume of a fix.
func blockNeedsSignIn(b *agent.Blocked) bool {
	if b == nil {
		return false
	}
	switch b.Reason {
	case agent.BlockSignIn, agent.BlockRevoked, agent.BlockConflict:
		return true
	default:
		return false
	}
}
