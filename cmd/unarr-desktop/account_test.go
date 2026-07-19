package main

import (
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

func TestPlanLabel(t *testing.T) {
	tests := []struct {
		name          string
		plan          string
		isPro         bool
		trialActive   bool
		trialDaysLeft int
		want          string
	}{
		{"pro plan", "pro", true, false, 0, "unarr+"},
		{"pro plan wins over trial", "pro", true, true, 5, "unarr+"},
		{"active trial with days left", "free", true, true, 4, "unarr+ trial · 4 days left"},
		{"active trial one day left", "free", true, true, 1, "unarr+ trial · 1 day left"},
		{"active trial without a day count", "free", true, true, 0, "unarr+ (trial)"},
		{"free", "free", false, false, 0, "Free"},
		{"trial flag without isPro", "free", false, true, 3, "Free"},
		// isPro is authoritative: a plan value this binary doesn't know
		// (future tier) must still render as paid — old shipped trays can't
		// be fixed server-side.
		{"unknown paid plan trusts isPro", "plus", true, false, 0, "unarr+"},
		{"empty plan", "", false, false, 0, "Free"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := planLabel(tt.plan, tt.isPro, tt.trialActive, tt.trialDaysLeft); got != tt.want {
				t.Errorf("planLabel(%q, %v, %v, %d) = %q, want %q", tt.plan, tt.isPro, tt.trialActive, tt.trialDaysLeft, got, tt.want)
			}
		})
	}
}

func TestUpgradeURL(t *testing.T) {
	// The CTA must target the SAME base the account was fetched from — the
	// caller passes it explicitly, so a config-file api_url (no env var set)
	// still produces a same-origin pricing link.
	tests := []struct{ base, want string }{
		{"https://unarr.app", "https://unarr.app/pricing?utm_source=unarr-desktop&utm_medium=tray&utm_campaign=upgrade"},
		{"http://localhost:3030", "http://localhost:3030/pricing?utm_source=unarr-desktop&utm_medium=tray&utm_campaign=upgrade"},
	}
	for _, tt := range tests {
		if got := upgradeURL(tt.base); got != tt.want {
			t.Errorf("upgradeURL(%q) = %q, want %q", tt.base, got, tt.want)
		}
	}
}

func TestAccountTitle(t *testing.T) {
	tests := []struct {
		name string
		info agent.AccountInfo
		want string
	}{
		{"pro", agent.AccountInfo{Email: "a@b.c", Plan: "pro", IsPro: true}, "Account: a@b.c — unarr+"},
		{"trial", agent.AccountInfo{Email: "a@b.c", Plan: "free", IsPro: true, TrialActive: true}, "Account: a@b.c — unarr+ (trial)"},
		{"free", agent.AccountInfo{Email: "a@b.c", Plan: "free"}, "Account: a@b.c — Free"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := accountTitle(&tt.info); got != tt.want {
				t.Errorf("accountTitle(%+v) = %q, want %q", tt.info, got, tt.want)
			}
		})
	}
}

func TestVersionTitle(t *testing.T) {
	tests := []struct {
		agentVersion, appVersion, want string
	}{
		{"1.5.0", "0.2.0", "Version: agent 1.5.0 · app 0.2.0"},
		{"unknown", "dev", "Version: agent unknown · app dev"},
	}
	for _, tt := range tests {
		if got := versionTitle(tt.agentVersion, tt.appVersion); got != tt.want {
			t.Errorf("versionTitle(%q, %q) = %q, want %q", tt.agentVersion, tt.appVersion, got, tt.want)
		}
	}
}
