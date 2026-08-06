package cmd

import (
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
)

// The regression, seen live on a real pro account: the par2 line said "not
// needed (usenet not in preferred_methods)" while the Methods group four rows
// below reported a healthy usenet server with ten connection slots.
//
// preferred_method = "auto" is the DEFAULT, and MethodOrder() returns nil for
// it. The old check read that nil as "usenet is not configured" and stood down.
// Usenet downloads were going to happen on that machine, and without par2 they
// arrive UNVERIFIED — the single thing this check exists to prevent.
func TestUsenetIsInPlayUnderAutoWhenTheAccountHasIt(t *testing.T) {
	cfg := config.Default()
	cfg.Download.PreferredMethod = "auto"
	cfg.Download.PreferredMethods = nil
	if order := cfg.Download.MethodOrder(); order != nil {
		t.Fatalf("test premise broken: MethodOrder() = %v, want nil for auto", order)
	}

	inPlay, why := usenetInPlay(&cfg, staticFeatures(agent.FeatureFlags{Usenet: true}))
	if !inPlay {
		t.Errorf("auto + account HAS usenet: par2 is needed, got not-needed (%s)", why)
	}

	inPlay, why = usenetInPlay(&cfg, staticFeatures(agent.FeatureFlags{Usenet: false}))
	if inPlay {
		t.Error("auto + account has NO usenet: par2 is not needed")
	}
	// The reason has to say which of the two it was, or the user cannot tell a
	// missing add-on from a config choice.
	if !strings.Contains(why, "add-on") {
		t.Errorf("reason = %q, want it to name the missing add-on", why)
	}
}

func TestUsenetInPlayHonoursAnExplicitList(t *testing.T) {
	cfg := config.Default()
	cfg.Download.PreferredMethods = []string{"usenet"}
	if inPlay, _ := usenetInPlay(&cfg, staticFeatures(agent.FeatureFlags{Usenet: false})); !inPlay {
		t.Error("explicitly requested usenet needs par2 whatever the account says")
	}

	cfg.Download.PreferredMethods = []string{"torrent"}
	inPlay, why := usenetInPlay(&cfg, staticFeatures(agent.FeatureFlags{Usenet: true}))
	if inPlay {
		t.Error("usenet absent from an explicit list: par2 not needed")
	}
	if !strings.Contains(why, "preferred_methods") {
		t.Errorf("reason = %q", why)
	}
}

// Offline, the check falls back to the config-only reading. A doctor run with
// no network must not start inventing warnings it cannot substantiate.
func TestUsenetInPlayFallsBackWhenTheAccountIsUnknown(t *testing.T) {
	cfg := config.Default()
	cfg.Download.PreferredMethods = []string{"usenet"}
	if inPlay, _ := usenetInPlay(&cfg, failingFeatures("no API key")); !inPlay {
		t.Error("offline + usenet explicitly requested: still needs par2")
	}

	cfg.Download.PreferredMethods = []string{"auto"}
	if inPlay, _ := usenetInPlay(&cfg, failingFeatures("no API key")); inPlay {
		t.Error("offline + auto: nothing is known, keep the old behaviour")
	}
}
