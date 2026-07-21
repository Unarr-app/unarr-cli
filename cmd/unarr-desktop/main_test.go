package main

import "testing"

// TestDispatchArgs pins the STRICT argv dispatch: only a bare invocation may
// start the tray. Everything unrecognized — typos, extra arguments, unknown
// flags — must map to modeUsageError (exit 2), never fall through to
// systray.Run: on a desktop that spawns a phantom tray, on a headless box it
// hangs waiting on DBus.
func TestDispatchArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantMode runMode
		wantRaw  string
	}{
		{"bare starts tray", nil, modeTray, ""},
		{"version", []string{"--version"}, modeVersion, ""},
		{"update", []string{"--update"}, modeUpdate, ""},
		{"help", []string{"--help"}, modeHelp, ""},
		{"help short", []string{"-h"}, modeHelp, ""},
		{"open with url", []string{"--open", "unarr://play?url=x"}, modeOpen, "unarr://play?url=x"},
		{"open equals form", []string{"--open=unarr://play?url=x"}, modeOpen, "unarr://play?url=x"},
		{"bare unarr link (%u substitution)", []string{"unarr://play?url=x"}, modeOpen, "unarr://play?url=x"},
		// Bare --open keeps its historical shape: modeOpen with an empty raw,
		// so runOpen prints its usage and exits 2 without starting a tray.
		{"open without url", []string{"--open"}, modeOpen, ""},
		// The phantom-tray regressions:
		{"typo'd flag", []string{"--updat"}, modeUsageError, ""},
		{"version with extra arg", []string{"--version", "extra"}, modeUsageError, ""},
		{"update with extra arg", []string{"--update", "now"}, modeUsageError, ""},
		{"help with extra arg", []string{"--help", "me"}, modeUsageError, ""},
		{"unknown flag", []string{"-v"}, modeUsageError, ""},
		{"non-flag garbage", []string{"start"}, modeUsageError, ""},
		{"unarr link plus extra arg", []string{"unarr://play?url=x", "extra"}, modeUsageError, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, raw := dispatchArgs(tt.args)
			if mode != tt.wantMode || raw != tt.wantRaw {
				t.Fatalf("dispatchArgs(%q) = (%v, %q), want (%v, %q)", tt.args, mode, raw, tt.wantMode, tt.wantRaw)
			}
		})
	}
}

// TestHubURL pins the "Configure agent (web)" deep-link. The tray menu can't be
// clicked in CI/headless, so the URL construction is the contract that gets
// tested: with an id the web lands on THAT machine's card; without one it must
// degrade to the historical generic hub rather than emitting `&agent=`, which
// would make every card a non-match and silently break the plain flow.
func TestHubURL(t *testing.T) {
	t.Setenv("UNARR_API_URL", "http://localhost:3028")

	tests := []struct {
		name    string
		agentID string
		want    string
	}{
		{
			name:    "no id falls back to the generic hub",
			agentID: "",
			want:    "http://localhost:3028/profile?tab=agents",
		},
		{
			name:    "known id deep-links to its card",
			agentID: "dev-local-agent-001",
			want:    "http://localhost:3028/profile?tab=agents&agent=dev-local-agent-001",
		},
		{
			name:    "uuid form",
			agentID: "f57ff97a-024c-43a2-934d-61c67abd79f8",
			want:    "http://localhost:3028/profile?tab=agents&agent=f57ff97a-024c-43a2-934d-61c67abd79f8",
		},
		{
			// A malformed id must never be able to inject extra query params
			// (or a fragment) into the URL the browser is handed.
			name:    "hostile id is query-escaped",
			agentID: "a&tab=billing#x y",
			want:    "http://localhost:3028/profile?tab=agents&agent=a%26tab%3Dbilling%23x+y",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hubURL(tt.agentID); got != tt.want {
				t.Fatalf("hubURL(%q) = %q, want %q", tt.agentID, got, tt.want)
			}
		})
	}
}

// TestHubURLDefaultBase guards the production default: without UNARR_API_URL the
// deep-link must point at the public app, not localhost.
func TestHubURLDefaultBase(t *testing.T) {
	t.Setenv("UNARR_API_URL", "")

	want := "https://unarr.app/profile?tab=agents&agent=abc"
	if got := hubURL("abc"); got != want {
		t.Fatalf("hubURL(\"abc\") = %q, want %q", got, want)
	}
}
