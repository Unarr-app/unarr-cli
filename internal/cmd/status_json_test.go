package cmd

import (
	"path/filepath"
	"testing"
)

// `unarr status --json` has always been accepted (root's persistent --json) and
// printed the usual text. A local --json on status shadowed it and turned that
// into "--json needs --sessions", breaking whoever already called it so.
func TestStatusJSONWithoutSessionsKeepsText(t *testing.T) {
	root := t.TempDir()
	t.Setenv("UNARR_CONFIG_DIR", filepath.Join(root, "cfg"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "data"))
	t.Setenv("UNARR_API_KEY", "") // not configured: status returns before any network call
	oldFile, oldLoaded, oldJSON := cfgFile, cfgLoaded, jsonOut
	cfgFile, cfgLoaded = "", false
	t.Cleanup(func() { cfgFile, cfgLoaded, jsonOut = oldFile, oldLoaded, oldJSON })

	rootCmd.SetArgs([]string{"status", "--json"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("unarr status --json: %v", err)
	}
	if !jsonOut {
		t.Fatal("--json must set root's persistent flag, not a local copy")
	}
}
