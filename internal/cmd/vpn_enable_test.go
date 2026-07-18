package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// withVPNTestConfig points the cmd package's config globals (cfgFile/appCfg/
// cfgLoaded — the same seam init/funnel/login use) at a fresh temp config file
// seeded with cfg, restoring them on cleanup. Returns the config path.
func withVPNTestConfig(t *testing.T, cfg config.Config) string {
	t.Helper()
	// Neutralize any inherited UNARR_* overrides so the API-key scenarios are
	// deterministic regardless of the operator's shell.
	t.Setenv("UNARR_API_KEY", "")
	t.Setenv("UNARR_API_URL", "")

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := config.Save(cfg, path); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	prevFile, prevCfg, prevLoaded := cfgFile, appCfg, cfgLoaded
	t.Cleanup(func() { cfgFile, appCfg, cfgLoaded = prevFile, prevCfg, prevLoaded })
	cfgFile = path
	cfgLoaded = false // force loadConfig to re-read from the temp file
	return path
}

// TestSetVPNEnabled covers the `unarr vpn enable/disable` config writer: the
// user-facing kill-switch toggle. A persist bug means "enabled" silently doesn't
// stick, or a disable is blocked by a spurious API-key requirement.
func TestSetVPNEnabled(t *testing.T) {
	t.Run("enable without an API key errors and persists nothing", func(t *testing.T) {
		cfg := config.Default()
		cfg.Auth.APIKey = ""
		cfg.Download.VPN.Enabled = false
		path := withVPNTestConfig(t, cfg)

		err := setVPNEnabled(true)
		if err == nil || !strings.Contains(err.Error(), "no API key") {
			t.Fatalf("err = %v, want a 'no API key' error", err)
		}
		reloaded, lerr := config.Load(path)
		if lerr != nil {
			t.Fatalf("reload: %v", lerr)
		}
		if reloaded.Download.VPN.Enabled {
			t.Error("VPN.Enabled persisted true despite the API-key error")
		}
	})

	t.Run("no-op when already at the target does not rewrite the file", func(t *testing.T) {
		cfg := config.Default()
		cfg.Auth.APIKey = "tc_key"
		cfg.Download.VPN.Enabled = true
		path := withVPNTestConfig(t, cfg)

		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := setVPNEnabled(true); err != nil {
			t.Fatalf("no-op enable errored: %v", err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Error("config file was re-written on a no-op toggle; want it untouched")
		}
	})

	t.Run("real enable persists and is reloadable", func(t *testing.T) {
		cfg := config.Default()
		cfg.Auth.APIKey = "tc_key"
		cfg.Download.VPN.Enabled = false
		path := withVPNTestConfig(t, cfg)

		if err := setVPNEnabled(true); err != nil {
			t.Fatalf("enable errored: %v", err)
		}
		reloaded, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if !reloaded.Download.VPN.Enabled {
			t.Error("VPN.Enabled did not persist to disk (kill-switch would silently stay off)")
		}
		// The in-memory cached config is refreshed too, so a later loadConfig() in
		// the same process sees the new value without a re-read.
		if !appCfg.Download.VPN.Enabled {
			t.Error("cached appCfg not updated after enable")
		}
	})

	t.Run("enable with a config_file set still persists (self-hosted precedence warning path)", func(t *testing.T) {
		cfg := config.Default()
		cfg.Auth.APIKey = "tc_key"
		cfg.Download.VPN.Enabled = false
		cfg.Download.VPN.ConfigFile = "/home/me/wg-personal.conf"
		path := withVPNTestConfig(t, cfg)

		if err := setVPNEnabled(true); err != nil {
			t.Fatalf("enable errored: %v", err)
		}
		reloaded, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if !reloaded.Download.VPN.Enabled {
			t.Error("VPN.Enabled did not persist with a config_file present")
		}
		// The self-hosted source is left intact by the enable toggle.
		if reloaded.Download.VPN.ConfigFile != "/home/me/wg-personal.conf" {
			t.Errorf("ConfigFile = %q, want it preserved", reloaded.Download.VPN.ConfigFile)
		}
	})

	t.Run("disable needs no API key and persists", func(t *testing.T) {
		cfg := config.Default()
		cfg.Auth.APIKey = "" // disabling must not require a key
		cfg.Download.VPN.Enabled = true
		path := withVPNTestConfig(t, cfg)

		if err := setVPNEnabled(false); err != nil {
			t.Fatalf("disable errored: %v", err)
		}
		reloaded, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if reloaded.Download.VPN.Enabled {
			t.Error("VPN.Enabled did not persist false after disable")
		}
	})
}
