package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	cfg := Default()

	if cfg.Auth.APIURL != "https://torrentclaw.com" {
		t.Errorf("default APIURL = %q, want https://torrentclaw.com", cfg.Auth.APIURL)
	}
	if cfg.Download.PreferredMethod != "auto" {
		t.Errorf("default PreferredMethod = %q, want auto", cfg.Download.PreferredMethod)
	}
	if cfg.Download.MaxConcurrent != 3 {
		t.Errorf("default MaxConcurrent = %d, want 3", cfg.Download.MaxConcurrent)
	}
	if cfg.General.Country != "US" {
		t.Errorf("default Country = %q, want US", cfg.General.Country)
	}
	if cfg.Daemon.HeartbeatInterval != "30s" {
		t.Errorf("default HeartbeatInterval = %q, want 30s", cfg.Daemon.HeartbeatInterval)
	}
}

func TestLoadMissingFile(t *testing.T) {
	cfg, err := Load("/nonexistent/path/config.toml")
	if err != nil {
		t.Fatalf("Load nonexistent should return defaults, got err: %v", err)
	}
	if cfg.Auth.APIURL != "https://torrentclaw.com" {
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
	if cfg.Auth.APIURL != "https://torrentclaw.com" {
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

func TestLoadInvalidTOML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	os.WriteFile(path, []byte(`not valid toml [[[`), 0o644)

	_, err := Load(path)
	if err == nil {
		t.Error("expected error for invalid TOML, got nil")
	}
}
