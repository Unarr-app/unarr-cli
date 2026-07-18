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
