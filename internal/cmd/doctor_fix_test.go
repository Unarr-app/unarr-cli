package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

func TestNormalizeAPIURL(t *testing.T) {
	tests := []struct {
		in          string
		wantURL     string
		wantChanged bool
	}{
		{"torrentclaw.com", "https://torrentclaw.com", true},
		{"https://torrentclaw.com/", "https://torrentclaw.com", true},
		{"https://torrentclaw.com/api/v1", "https://torrentclaw.com", true},
		{"https://torrentclaw.com/api", "https://torrentclaw.com", true},
		{"https://torrentclaw.com", "https://torrentclaw.com", false},
		{"http://localhost:3030", "http://localhost:3030", false},
		{"ftp://x.com", "https://x.com", true},
		{"  https://x.com/  ", "https://x.com", true},
		{"", "", false},
		{"   ", "   ", false}, // whitespace-only left as-is, not "fixed" into garbage
		// Scheme case is normalized deterministically.
		{"HTTPS://x.com", "https://x.com", true},
		{"HTTP://x.com", "http://x.com", true},
		// Userinfo (self-hoster basic auth) and ports must survive cleaning.
		{"https://user:pass@x.com/", "https://user:pass@x.com", true},
		{"user:pass@x.com", "https://user:pass@x.com", true},
		{"https://x.com:8443/api/v1", "https://x.com:8443", true},
		{"http://[::1]:3030", "http://[::1]:3030", false},
		// A stray query/fragment on a BASE url is dropped with the path.
		{"https://x.com/?utm=1", "https://x.com", true},
		// Only a literal /api segment is stripped — not a prefix-alike.
		{"https://x.com/apiary", "https://x.com/apiary", false},
		{"https://x.com/unarr", "https://x.com/unarr", false},
	}
	for _, tt := range tests {
		gotURL, gotChanged := normalizeAPIURL(tt.in)
		if gotURL != tt.wantURL || gotChanged != tt.wantChanged {
			t.Errorf("normalizeAPIURL(%q) = (%q, %v), want (%q, %v)",
				tt.in, gotURL, gotChanged, tt.wantURL, tt.wantChanged)
		}
	}
}

// healthyConfigFile writes a valid 0600 config.toml and returns its path, so
// planRepairs sees no file-level problems unless a test creates one.
func healthyConfigFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := config.Save(config.Default(), path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlanRepairs_HealthyConfigIsNoOp(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Download.Dir = dir // exists
	// Default() api_url is clean and mirrors are populated → nothing to repair.
	if reps := planRepairs(&cfg, healthyConfigFile(t)); len(reps) != 0 {
		t.Fatalf("expected 0 repairs for healthy config, got %d: %+v", len(reps), reps)
	}
}

func TestPlanRepairs_FixesMalformedURLAndEmptyMirrors(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{}
	cfg.Auth.APIURL = "torrentclaw.com/api/v1" // no scheme + /api suffix
	cfg.Auth.Mirrors = nil                     // empty
	cfg.Download.Dir = dir

	reps := planRepairs(&cfg, healthyConfigFile(t))
	if len(reps) != 2 {
		t.Fatalf("expected 2 repairs, got %d: %+v", len(reps), reps)
	}
	for _, r := range reps {
		if err := r.apply(); err != nil {
			t.Fatalf("apply %q: %v", r.desc, err)
		}
	}
	if cfg.Auth.APIURL != "https://torrentclaw.com" {
		t.Errorf("api_url = %q, want https://torrentclaw.com", cfg.Auth.APIURL)
	}
	if len(cfg.Auth.Mirrors) == 0 {
		t.Errorf("mirrors still empty after repair")
	}
}

func TestPlanRepairs_CreatesMissingDownloadDir(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "downloads", "unarr")
	cfg := config.Default()
	cfg.Download.Dir = missing

	reps := planRepairs(&cfg, healthyConfigFile(t))
	if len(reps) != 1 {
		t.Fatalf("expected 1 repair (create dir), got %d: %+v", len(reps), reps)
	}
	if err := reps[0].apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if fi, err := os.Stat(missing); err != nil || !fi.IsDir() {
		t.Errorf("download dir not created: err=%v", err)
	}
}

func TestPlanRepairs_SetsDefaultDownloadDirWhenEmpty(t *testing.T) {
	cfg := config.Default()
	cfg.Download.Dir = ""

	reps := planRepairs(&cfg, healthyConfigFile(t))
	if len(reps) != 1 {
		t.Fatalf("expected 1 repair (set default dir), got %d: %+v", len(reps), reps)
	}
	if err := reps[0].apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if cfg.Download.Dir == "" {
		t.Errorf("download dir still empty after repair")
	}
}

func TestBackupConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	bak, err := backupConfigFile(path, 1700000000)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if bak == "" {
		t.Fatal("expected a backup path")
	}
	data, err := os.ReadFile(bak)
	if err != nil || string(data) != "hello" {
		t.Errorf("backup content = %q (err %v), want %q", data, err, "hello")
	}

	// Missing source → no backup, no error.
	bak2, err := backupConfigFile(filepath.Join(dir, "nope.toml"), 1700000000)
	if err != nil || bak2 != "" {
		t.Errorf("missing source: got (%q, %v), want (\"\", nil)", bak2, err)
	}
}

func TestPlanRepairs_TightensLooseConfigPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := config.Save(config.Default(), path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Download.Dir = dir

	reps := planRepairs(&cfg, path)
	if len(reps) != 1 {
		t.Fatalf("expected 1 repair (chmod), got %d: %+v", len(reps), reps)
	}
	if err := reps[0].apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm after repair = %04o, want 0600", perm)
	}

	// Idempotent: second plan finds nothing.
	if reps := planRepairs(&cfg, path); len(reps) != 0 {
		t.Errorf("expected 0 repairs after chmod, got %d: %+v", len(reps), reps)
	}
}

func TestConfigFileBroken(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing.toml")
	if configFileBroken(missing) {
		t.Errorf("missing file must NOT count as broken (Load defaults it)")
	}

	valid := filepath.Join(dir, "valid.toml")
	if err := config.Save(config.Default(), valid); err != nil {
		t.Fatal(err)
	}
	if configFileBroken(valid) {
		t.Errorf("valid TOML flagged as broken")
	}

	garbage := filepath.Join(dir, "garbage.toml")
	if err := os.WriteFile(garbage, []byte("[[[not toml ==="), 0o600); err != nil {
		t.Fatal(err)
	}
	if !configFileBroken(garbage) {
		t.Errorf("unparseable TOML not flagged as broken")
	}
}
