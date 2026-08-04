package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/logging"
)

// loadTOML writes a config file and loads it, so a test exercises the real
// decode + applyDefaults path rather than a hand-built struct.
func loadTOML(t *testing.T, body string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestLogKeysDefaultForConfigsThatPredateThem(t *testing.T) {
	// The upgrade case: a config written before these keys existed must come
	// back with every one of them resolved, so `unarr logs` and the ring walk
	// the same numbers whether or not the user ever edited their TOML. The size
	// budget resolves to 0 — rotation is opt-in, see
	// TestLogRotationIsOffByDefault — while the slot count, level and format
	// keep real defaults.
	cfg := loadTOML(t, "[daemon]\nauto_upgrade = true\n")

	if cfg.Daemon.LogMaxSizeMB != defaultLogMaxSizeMB {
		t.Fatalf("log_max_size_mb = %d, want %d", cfg.Daemon.LogMaxSizeMB, defaultLogMaxSizeMB)
	}
	if cfg.Daemon.LogMaxFiles != defaultLogMaxFiles {
		t.Fatalf("log_max_files = %d, want %d", cfg.Daemon.LogMaxFiles, defaultLogMaxFiles)
	}
	if cfg.Daemon.LogLevel != defaultLogLevel {
		t.Fatalf("log_level = %q, want %q", cfg.Daemon.LogLevel, defaultLogLevel)
	}
	if cfg.Daemon.LogFormat != defaultLogFormat {
		t.Fatalf("log_format = %q, want %q", cfg.Daemon.LogFormat, defaultLogFormat)
	}
}

// TestLogRingDefaultsAgreeWithTheLoggingPackage is the guard the old "the
// logging package asserts the same numbers from its own side" comment promised
// and never delivered. The slot count now has exactly one definition
// (logging.DefaultMaxFiles) and this pins the whole resolution chain to it: if
// anyone re-literalises the number here, or the two drift for any other reason,
// `unarr logs` and `unarr clean` would walk a different number of slots than
// the rotator writes — rotated history silently invisible, or swept files that
// were supposed to be kept.
func TestLogRingDefaultsAgreeWithTheLoggingPackage(t *testing.T) {
	cfg := loadTOML(t, "[daemon]\nauto_upgrade = true\n")
	if cfg.Daemon.LogMaxFiles != logging.DefaultMaxFiles {
		t.Fatalf("a config that omits log_max_files resolves to %d slots, but "+
			"logging falls back to %d — the two defaults have drifted apart",
			cfg.Daemon.LogMaxFiles, logging.DefaultMaxFiles)
	}
	// The other side of the same number: what logging walks when MaxFiles is
	// left unset has to be the same ring the config describes.
	if got := len(logging.RotatedPaths("/x/unarr.log", 0)); got != cfg.Daemon.LogMaxFiles {
		t.Fatalf("logging walks %d rotated slots with MaxFiles unset, config resolves %d",
			got, cfg.Daemon.LogMaxFiles)
	}
}

func TestLogRotationCanBeDisabledExplicitly(t *testing.T) {
	// 0 is a meaningful value here ("never rotate"), so it must survive
	// applyDefaults — the reason the check is IsDefined and not a zero test.
	cfg := loadTOML(t, "[daemon]\nlog_max_size_mb = 0\n")
	if cfg.Daemon.LogMaxSizeMB != 0 {
		t.Fatalf("log_max_size_mb = %d, want the explicit 0 respected", cfg.Daemon.LogMaxSizeMB)
	}
}

func TestLogKeysAreCarriedThrough(t *testing.T) {
	cfg := loadTOML(t, "[daemon]\nlog_max_size_mb = 5\nlog_max_files = 7\nlog_level = \"debug\"\nlog_format = \"json\"\n")
	if cfg.Daemon.LogMaxSizeMB != 5 || cfg.Daemon.LogMaxFiles != 7 {
		t.Fatalf("sizes not read: %+v", cfg.Daemon)
	}
	if cfg.Daemon.LogLevel != "debug" || cfg.Daemon.LogFormat != "json" {
		t.Fatalf("level/format not read: %+v", cfg.Daemon)
	}
}

func TestDefaultConfigShipsTheLogRing(t *testing.T) {
	d := Default().Daemon
	if d.LogMaxSizeMB != defaultLogMaxSizeMB || d.LogMaxFiles != defaultLogMaxFiles {
		t.Fatalf("Default() must spell out the log ring, got %+v", d)
	}
}

func TestBadLogValuesAreReportedAsIssues(t *testing.T) {
	cfg := Default()
	cfg.Daemon.LogLevel = "warm"
	cfg.Daemon.LogFormat = "yaml"
	cfg.Daemon.LogMaxFiles = -1

	keys := map[string]bool{}
	for _, issue := range cfg.ValueIssues() {
		keys[issue.Key] = true
	}
	for _, want := range []string{"daemon.log_level", "daemon.log_format", "daemon.log_max_files"} {
		if !keys[want] {
			t.Fatalf("%s was not reported as an invalid value (got %v)", want, keys)
		}
	}
}

func TestGoodLogValuesRaiseNoIssue(t *testing.T) {
	cfg := Default()
	for _, issue := range cfg.ValueIssues() {
		t.Fatalf("the default config must be clean, got %s", issue.Message)
	}
}
