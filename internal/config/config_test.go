package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	cfg := Default()

	if cfg.Auth.APIURL != "https://unarr.app" {
		t.Errorf("default APIURL = %q, want https://unarr.app", cfg.Auth.APIURL)
	}
	if cfg.Download.PreferredMethod != "auto" {
		t.Errorf("default PreferredMethod = %q, want auto", cfg.Download.PreferredMethod)
	}
	if cfg.Download.MaxConcurrent != 3 {
		t.Errorf("default MaxConcurrent = %d, want 3", cfg.Download.MaxConcurrent)
	}
	if cfg.Download.MaxStreamSessions != 1 {
		t.Errorf("default MaxStreamSessions = %d, want 1", cfg.Download.MaxStreamSessions)
	}
	if cfg.General.Country != "US" {
		t.Errorf("default Country = %q, want US", cfg.General.Country)
	}
	if cfg.Daemon.StatusInterval != "" {
		t.Errorf("default StatusInterval = %q, want empty", cfg.Daemon.StatusInterval)
	}
}

func TestLoadMissingFile(t *testing.T) {
	cfg, err := Load("/nonexistent/path/config.toml")
	if err != nil {
		t.Fatalf("Load nonexistent should return defaults, got err: %v", err)
	}
	if cfg.Auth.APIURL != "https://unarr.app" {
		t.Errorf("missing file should return default APIURL, got %q", cfg.Auth.APIURL)
	}
}

func TestSaveAndLoad(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")

	cfg := Default()
	cfg.Auth.APIKey = "tc_test123"
	cfg.Auth.APIURL = "https://custom.example.com"
	cfg.General.Country = "ES"
	cfg.Download.Dir = "/media/downloads"
	cfg.Agent.ID = "agent-uuid-123"
	cfg.Agent.Name = "Test Machine"

	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// File should exist
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("config file was not created")
	}

	// No .tmp file left behind
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file was not cleaned up")
	}

	// Load it back
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if loaded.Auth.APIKey != "tc_test123" {
		t.Errorf("APIKey = %q, want tc_test123", loaded.Auth.APIKey)
	}
	if loaded.Auth.APIURL != "https://custom.example.com" {
		t.Errorf("APIURL = %q, want https://custom.example.com", loaded.Auth.APIURL)
	}
	if loaded.General.Country != "ES" {
		t.Errorf("Country = %q, want ES", loaded.General.Country)
	}
	if loaded.Download.Dir != "/media/downloads" {
		t.Errorf("Dir = %q, want /media/downloads", loaded.Download.Dir)
	}
	if loaded.Agent.ID != "agent-uuid-123" {
		t.Errorf("AgentID = %q, want agent-uuid-123", loaded.Agent.ID)
	}
	if loaded.Agent.Name != "Test Machine" {
		t.Errorf("AgentName = %q, want Test Machine", loaded.Agent.Name)
	}
}

func TestLoadPreservesDefaults(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")

	// Write partial config (only auth section)
	os.WriteFile(path, []byte(`[auth]
api_key = "tc_partial"
`), 0o644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Auth.APIKey != "tc_partial" {
		t.Errorf("APIKey = %q, want tc_partial", cfg.Auth.APIKey)
	}
	// Defaults should be preserved for missing sections
	if cfg.Auth.APIURL != "https://unarr.app" {
		t.Errorf("APIURL should default, got %q", cfg.Auth.APIURL)
	}
	if cfg.Download.MaxConcurrent != 3 {
		t.Errorf("MaxConcurrent should default to 3, got %d", cfg.Download.MaxConcurrent)
	}
	if cfg.General.Country != "US" {
		t.Errorf("Country should default to US, got %q", cfg.General.Country)
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	cfg := Default()

	t.Setenv("UNARR_API_KEY", "tc_env_key")
	t.Setenv("UNARR_API_URL", "https://env.example.com")
	t.Setenv("UNARR_COUNTRY", "DE")
	t.Setenv("UNARR_DOWNLOAD_DIR", "/env/downloads")

	cfg.ApplyEnvOverrides()

	if cfg.Auth.APIKey != "tc_env_key" {
		t.Errorf("APIKey = %q, want tc_env_key", cfg.Auth.APIKey)
	}
	if cfg.Auth.APIURL != "https://env.example.com" {
		t.Errorf("APIURL = %q, want https://env.example.com", cfg.Auth.APIURL)
	}
	if cfg.General.Country != "DE" {
		t.Errorf("Country = %q, want DE", cfg.General.Country)
	}
	if cfg.Download.Dir != "/env/downloads" {
		t.Errorf("Dir = %q, want /env/downloads", cfg.Download.Dir)
	}
}

func TestSaveCreatesDirectory(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "nested", "deep", "config.toml")

	cfg := Default()
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save with nested dir failed: %v", err)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("config file was not created in nested dir")
	}
}

func TestParseSpeed(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"0", 0},
		{"", 0},
		{"10MB", 10 * 1024 * 1024},
		{"500KB", 500 * 1024},
		{"1GB", 1024 * 1024 * 1024},
		{"1.5MB", int64(1.5 * 1024 * 1024)},
		{"10mb", 10 * 1024 * 1024},
		{"1024", 1024},
	}

	for _, tt := range tests {
		got, err := ParseSpeed(tt.input)
		if err != nil {
			t.Errorf("ParseSpeed(%q) error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseSpeed(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}

	// Error cases
	if _, err := ParseSpeed("abc"); err == nil {
		t.Error("ParseSpeed(\"abc\") should error")
	}
	if _, err := ParseSpeed("-5MB"); err == nil {
		t.Error("ParseSpeed(\"-5MB\") should error")
	}
}

func TestLoadMinimalTOMLAppliesStreamingDefaults(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")

	// Minimal config — only auth + agent. Nothing about webrtc / transcode.
	os.WriteFile(path, []byte(`[auth]
api_key = "tc_minimal"

[agent]
id = "agent-uuid"
name = "Test"
`), 0o644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Transcode should be on by default.
	if !cfg.Download.Transcode.Enabled {
		t.Error("Transcode.Enabled should default to true when [downloads.transcode] is absent")
	}
	if cfg.Download.Transcode.HWAccel != "auto" {
		t.Errorf("Transcode.HWAccel = %q, want auto", cfg.Download.Transcode.HWAccel)
	}
	if cfg.Download.Transcode.Preset != "" {
		// Default is now empty — engine.ResolveEncoderProfile picks
		// "superfast" on libx264 for first-start latency. Users
		// wanting better quality override in config.toml.
		t.Errorf("Transcode.Preset = %q, want empty", cfg.Download.Transcode.Preset)
	}
	if cfg.Download.Transcode.MaxConcurrent != 2 {
		t.Errorf("Transcode.MaxConcurrent = %d, want 2", cfg.Download.Transcode.MaxConcurrent)
	}
}

func TestLoadRespectsExplicitlyDisabledStreaming(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")

	// User explicitly opted out of transcode. Defaults must NOT override
	// it — that would silently re-enable a feature the user disabled.
	os.WriteFile(path, []byte(`[downloads.transcode]
enabled = false
`), 0o644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Download.Transcode.Enabled {
		t.Error("Transcode.Enabled = true, want false (user explicitly disabled)")
	}
}

func TestLoadMaxStreamSessions(t *testing.T) {
	tmp := t.TempDir()

	// Config predating the key → coerced to the personal-agent default of 1.
	missing := filepath.Join(tmp, "missing.toml")
	os.WriteFile(missing, []byte(`[auth]
api_key = "tc_x"
`), 0o644)
	cfg, err := Load(missing)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Download.MaxStreamSessions != 1 {
		t.Errorf("MaxStreamSessions (key absent) = %d, want 1", cfg.Download.MaxStreamSessions)
	}

	// Explicit 0 (or negative) must also coerce to 1, never 0 (which would
	// evict every session on register and serve nobody).
	zero := filepath.Join(tmp, "zero.toml")
	os.WriteFile(zero, []byte(`[downloads]
max_stream_sessions = 0
`), 0o644)
	if cfg, err = Load(zero); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Download.MaxStreamSessions != 1 {
		t.Errorf("MaxStreamSessions (0) = %d, want 1", cfg.Download.MaxStreamSessions)
	}

	// A shared/server agent raising it is honoured verbatim.
	five := filepath.Join(tmp, "five.toml")
	os.WriteFile(five, []byte(`[downloads]
max_stream_sessions = 5
`), 0o644)
	if cfg, err = Load(five); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Download.MaxStreamSessions != 5 {
		t.Errorf("MaxStreamSessions (5) = %d, want 5", cfg.Download.MaxStreamSessions)
	}
}

func TestLoadUsenetStreaming(t *testing.T) {
	tmp := t.TempDir()

	// Absent key → OFF: on-the-fly usenet streaming is strictly opt-in, so a
	// config predating the feature keeps the safe batch-download behaviour.
	missing := filepath.Join(tmp, "missing.toml")
	os.WriteFile(missing, []byte(`[auth]
api_key = "tc_x"
`), 0o644)
	cfg, err := Load(missing)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Download.UsenetStreaming {
		t.Error("UsenetStreaming (key absent) = true, want false (opt-in)")
	}

	// Explicit opt-in is honoured.
	on := filepath.Join(tmp, "on.toml")
	os.WriteFile(on, []byte(`[downloads]
usenet_streaming = true
`), 0o644)
	if cfg, err = Load(on); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.Download.UsenetStreaming {
		t.Error("UsenetStreaming (=true) = false, want true")
	}
}

func TestLoadSeedingDefaultsOff(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")

	// No [downloads] seeding keys — seeding must stay off by default.
	os.WriteFile(path, []byte(`[auth]
api_key = "tc_x"
`), 0o644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Download.SeedEnabled {
		t.Error("SeedEnabled should default to false")
	}
	if cfg.Download.SeedRatio != 0 {
		t.Errorf("SeedRatio = %v, want 0", cfg.Download.SeedRatio)
	}
	if cfg.Download.SeedTime != "" {
		t.Errorf("SeedTime = %q, want empty", cfg.Download.SeedTime)
	}
}

func TestLoadSeedingExplicit(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")

	os.WriteFile(path, []byte(`[downloads]
seed_enabled = true
seed_ratio = 2.0
seed_time = "24h"
`), 0o644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.Download.SeedEnabled {
		t.Error("SeedEnabled = false, want true")
	}
	if cfg.Download.SeedRatio != 2.0 {
		t.Errorf("SeedRatio = %v, want 2.0", cfg.Download.SeedRatio)
	}
	if cfg.Download.SeedTime != "24h" {
		t.Errorf("SeedTime = %q, want 24h", cfg.Download.SeedTime)
	}
}

// TestLoadVPNRequired locks in the kill-switch default-behavior invariant:
// [downloads.vpn] required round-trips when set, and an omitted key stays false
// (so an existing config that predates the key changes nothing).
func TestLoadVPNRequired(t *testing.T) {
	tmp := t.TempDir()

	on := filepath.Join(tmp, "on.toml")
	os.WriteFile(on, []byte("[downloads.vpn]\nrequired = true\n"), 0o644)
	cfg, err := Load(on)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.Download.VPN.Required {
		t.Error("VPN.Required = false, want true (explicitly set)")
	}

	off := filepath.Join(tmp, "off.toml")
	os.WriteFile(off, []byte("[downloads]\ndir = \"/tmp/dl\"\n"), 0o644)
	if cfg, err = Load(off); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Download.VPN.Required {
		t.Error("VPN.Required (key absent) = true, want false (default = current behavior)")
	}
}

func TestLoadInvalidTOML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	os.WriteFile(path, []byte(`not valid toml [[[`), 0o644)

	_, err := Load(path)
	if err == nil {
		t.Error("expected error for invalid TOML, got nil")
	}
}

// TestSaveLoadVPNRoundTrip locks the full [downloads.vpn] block through a
// Save→Load cycle: enabled, required (the fail-closed kill-switch flag), and
// config_file (self-hosted mode) must all survive a marshal/unmarshal. A drop
// here would silently disable the kill-switch or lose the self-hosted source.
func TestSaveLoadVPNRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")

	cfg := Default()
	cfg.Download.VPN.Enabled = true
	cfg.Download.VPN.Required = true
	cfg.Download.VPN.ConfigFile = "/etc/wireguard/wg0.conf"

	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.Download.VPN.Enabled {
		t.Error("VPN.Enabled did not survive the Save→Load round-trip")
	}
	if !loaded.Download.VPN.Required {
		t.Error("VPN.Required (kill-switch flag) did not survive the Save→Load round-trip")
	}
	if loaded.Download.VPN.ConfigFile != "/etc/wireguard/wg0.conf" {
		t.Errorf("VPN.ConfigFile = %q, want /etc/wireguard/wg0.conf", loaded.Download.VPN.ConfigFile)
	}
}

// TestLoadVPNSelfHostedPrecedenceAndRequired: a self-hosted config (config_file
// set) is the daemon's preferred source over a managed fetch, so config_file must
// load intact; Required (the flag the whole kill-switch keys off) must survive a
// partial TOML that touches ONLY the VPN block; and unrelated defaults must still
// be applied by the merge.
func TestLoadVPNSelfHostedPrecedenceAndRequired(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	os.WriteFile(path, []byte(`[downloads.vpn]
config_file = "/home/me/wg-personal.conf"
required = true
`), 0o644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Self-hosted source present (daemon prefers it over a managed fetch).
	if cfg.Download.VPN.ConfigFile != "/home/me/wg-personal.conf" {
		t.Errorf("VPN.ConfigFile = %q, want the self-hosted path", cfg.Download.VPN.ConfigFile)
	}
	// Kill-switch flag survives a partial-TOML load...
	if !cfg.Download.VPN.Required {
		t.Error("VPN.Required dropped by a partial-TOML Load — the kill-switch would silently disable")
	}
	// ...Enabled defaults to false when its key is absent (self-hosted needs no managed fetch)...
	if cfg.Download.VPN.Enabled {
		t.Error("VPN.Enabled = true, want false when the key is absent")
	}
	// ...and unrelated defaults are still applied (the merge didn't clobber them).
	if cfg.Download.MaxConcurrent != 3 {
		t.Errorf("MaxConcurrent = %d, want default 3 (partial load must preserve defaults)", cfg.Download.MaxConcurrent)
	}
	if cfg.Auth.APIURL != "https://unarr.app" {
		t.Errorf("APIURL = %q, want default https://unarr.app", cfg.Auth.APIURL)
	}
}
