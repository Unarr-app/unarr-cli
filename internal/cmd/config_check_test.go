package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// loadTestConfig writes a TOML body to a temp file and loads it, so the
// resulting Config carries the same unknown-key diagnostics the CLI sees.
func loadTestConfig(t *testing.T, body string) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func TestConfigKeysCheckResultClean(t *testing.T) {
	cfg := loadTestConfig(t, "[downloads]\ndir = \"/data\"\nmin_free_disk_mb = 1024\n")
	msg, err := configKeysCheckResult(cfg)
	if err != nil {
		t.Fatalf("clean config must not fail the check: %v", err)
	}
	if strings.HasPrefix(msg, "!") {
		t.Errorf("clean config must not warn, got %q", msg)
	}
}

func TestConfigKeysCheckResultWarns(t *testing.T) {
	cfg := loadTestConfig(t, "[downloads]\nmin_free_disk_gb = 5\n")
	msg, err := configKeysCheckResult(cfg)
	// An unknown key is inert: WARN ("!"-prefixed, like par2CheckResult), never
	// a doctor failure.
	if err != nil {
		t.Fatalf("unknown keys must warn, not fail: %v", err)
	}
	if !strings.HasPrefix(msg, "!") {
		t.Fatalf("expected a WARN-prefixed message, got %q", msg)
	}
	if !strings.Contains(msg, `unknown key "downloads.min_free_disk_gb"`) {
		t.Errorf("message does not name the key: %q", msg)
	}
	if !strings.Contains(msg, `did you mean "downloads.min_free_disk_mb"?`) {
		t.Errorf("message does not suggest the fix: %q", msg)
	}
}

func TestUnknownKeyWarningsOnePerKey(t *testing.T) {
	cfg := loadTestConfig(t, "[auth]\napi_ky = \"k\"\n\n[library]\nworkerz = 4\n")
	got := unknownKeyWarnings(cfg)
	if len(got) != 2 {
		t.Fatalf("want one warning per key, got %v", got)
	}
}

// TestConfigCheckSubcommandRegistered guards the wiring: `unarr config check`
// must resolve to the validator, not to the interactive category menu.
func TestConfigCheckSubcommandRegistered(t *testing.T) {
	root := newConfigCmd()
	cmd, _, err := root.Find([]string{"check"})
	if err != nil {
		t.Fatalf("find `config check`: %v", err)
	}
	if cmd.Name() != "check" {
		t.Fatalf("`config check` resolved to %q", cmd.Name())
	}
	if cmd.RunE == nil {
		t.Error("`config check` has no RunE")
	}
	if err := cmd.Args(cmd, []string{"extra"}); err == nil {
		t.Error("`config check` should reject positional arguments")
	}
}
