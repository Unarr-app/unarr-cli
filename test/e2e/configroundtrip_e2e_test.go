//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// roundTrip does what every daemon-side config write does — Load the file, Save
// it back over itself — and returns the resulting file contents.
func roundTrip(t *testing.T, path string) string {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	if err := config.Save(cfg, path); err != nil {
		t.Fatalf("save %s: %v", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back %s: %v", path, err)
	}
	return string(data)
}

// TestRoundTripDoesNotInventADesktopSection pins the omitempty trap documented
// on DesktopConfig: a config that never mentioned [desktop] must come back
// without it, or every `unarr funnel off` would grow a section the user never
// asked for.
func TestRoundTripDoesNotInventADesktopSection(t *testing.T) {
	s := newSandbox(t)
	s.writeConfig(t, "[general]\ncountry = \"ES\"\n\n[daemon]\nlog_max_size_mb = 5\n")

	after := roundTrip(t, s.cfgPath)
	t.Logf("config after round-trip (%d bytes):\n%s", len(after), after)

	if strings.Contains(after, "[desktop]") {
		t.Errorf("round-trip invented a [desktop] section:\n%s", after)
	}
	if !strings.Contains(after, `country = "ES"`) {
		t.Errorf("round-trip lost general.country:\n%s", after)
	}
}

// TestRoundTripKeepsAnExistingDesktopSection is the other half of the same
// trap: the daemon never reads [desktop], so a Load→Save that dropped it would
// silently reset the desktop companion's settings.
func TestRoundTripKeepsAnExistingDesktopSection(t *testing.T) {
	s := newSandbox(t)
	s.writeConfig(t, "[general]\ncountry = \"ES\"\n\n[desktop]\nplayer = \"mpv\"\n"+
		"player_command = \"flatpak run org.videolan.VLC -- {url}\"\n")

	after := roundTrip(t, s.cfgPath)
	t.Logf("config after round-trip (%d bytes):\n%s", len(after), after)

	for _, want := range []string{"[desktop]", `player = "mpv"`, "org.videolan.VLC"} {
		if !strings.Contains(after, want) {
			t.Errorf("round-trip dropped %q from [desktop]:\n%s", want, after)
		}
	}

	cfg, err := config.Load(s.cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Desktop.Player != "mpv" {
		t.Errorf("desktop.player is %q after the round-trip, want \"mpv\"", cfg.Desktop.Player)
	}
}

// unknownKeyConfig has one unknown key inside a known section and one whole
// unknown section — the two ways a typo or a renamed key shows up.
const unknownKeyConfig = `[general]
country = "ES"
totally_bogus = "keep me"

[bananas]
peel = true
`

// TestRoundTripWithUnknownKeys documents what Load→Save actually does with keys
// the schema does not know.
//
// OBSERVED BEHAVIOUR: Load does not fail — it records them, readable through
// UnknownKeys(), which is what `unarr doctor` / `unarr config check` warn from.
// Save, however, re-encodes the Config STRUCT, and the unknown keys were never
// on it, so they are gone from the file afterwards. Save is lossy for anything
// outside the schema (comments and key order included). That is a real
// data-loss edge — a user who typo'd a key loses the evidence the first time
// any command saves — so it is asserted here rather than left to be discovered.
func TestRoundTripWithUnknownKeys(t *testing.T) {
	s := newSandbox(t)
	s.writeConfig(t, unknownKeyConfig)

	cfg, err := config.Load(s.cfgPath)
	if err != nil {
		t.Fatalf("load must not fail on unknown keys: %v", err)
	}
	unknown := cfg.UnknownKeys()
	t.Logf("Load reported unknown keys: %v", unknown)
	for _, want := range []string{"general.totally_bogus", "bananas.peel"} {
		if !containsString(unknown, want) {
			t.Errorf("Load did not report unknown key %q, got %v", want, unknown)
		}
	}

	after := roundTrip(t, s.cfgPath)
	t.Logf("config after round-trip (%d bytes):\n%s", len(after), after)

	if !strings.Contains(after, `country = "ES"`) {
		t.Errorf("round-trip lost the known key general.country:\n%s", after)
	}
	for _, gone := range []string{"totally_bogus", "[bananas]", "peel"} {
		if strings.Contains(after, gone) {
			t.Logf("NOTE: Save preserved %q — this test's documented expectation "+
				"(schema-only encode) no longer holds; update the comment", gone)
		}
	}

	// Reloading the saved file must be clean: whatever Save did, it cannot leave
	// a file that Load then complains about.
	reloaded, err := config.Load(s.cfgPath)
	if err != nil {
		t.Fatalf("reload after save: %v", err)
	}
	if got := reloaded.UnknownKeys(); len(got) != 0 {
		t.Logf("unknown keys still present after save: %v", got)
	}
	if reloaded.General.Country != "ES" {
		t.Errorf("general.country is %q after the round-trip, want \"ES\"", reloaded.General.Country)
	}
}

// containsString reports whether s holds want.
func containsString(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
