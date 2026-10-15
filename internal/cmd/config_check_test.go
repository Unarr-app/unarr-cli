package cmd

import (
	"errors"
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

// runConfigCheckOn runs `unarr config check --config <body written to a file>`
// as a fresh process would, and returns its error and config.Load's own.
func runConfigCheckOn(t *testing.T, body string) (checkErr, loadErr error) {
	t.Helper()
	for _, key := range []string{"UNARR_API_KEY", "UNARR_API_URL", "UNARR_DOWNLOAD_DIR", "UNARR_COUNTRY"} {
		t.Setenv(key, "")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	previous, loaded, previousFile, previousError := appCfg, cfgLoaded, cfgFile, errCfgLoad
	t.Cleanup(func() { appCfg, cfgLoaded, cfgFile, errCfgLoad = previous, loaded, previousFile, previousError })
	cfgFile, cfgLoaded, errCfgLoad = path, false, nil
	_, loadErr = config.Load(path)
	return runConfigCheck(), loadErr
}

// A config.toml config.Load rejects used to come back "no problems found" with
// exit 0: loadConfig fell back to the defaults, and those check clean. It must
// fail with the error the daemon refuses to start with.
func TestConfigCheckFailsOnConfigThatDoesNotLoad(t *testing.T) {
	cases := map[string]string{
		"unterminated table header": "[auth\napi_key = \"k\"\n",
		"unterminated string":       "[auth]\napi_key = \"k\n",
		"duplicate key":             "[auth]\napi_key = \"a\"\napi_key = \"b\"\n",
		"string for an integer":     "[downloads]\nmax_concurrent = \"three\"\n",
		"string for a list":         "[auth]\nmirrors = \"https://m.example\"\n",
		"table for a string":        "[auth]\napi_url = { host = \"x\" }\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			checkErr, loadErr := runConfigCheckOn(t, body)
			if loadErr == nil {
				t.Fatalf("fixture must be one config.Load rejects:\n%s", body)
			}
			want := "config could not be loaded: " + loadErr.Error() // daemon.go's wording
			if checkErr == nil || checkErr.Error() != want {
				t.Fatalf("config check = %v, want %q", checkErr, want)
			}
			if !errors.Is(checkErr, errCfgLoad) {
				t.Errorf("config check must wrap the load error, got %v", checkErr)
			}
		})
	}
	if checkErr, loadErr := runConfigCheckOn(t, "[downloads]\nmax_concurrent = 2\n"); loadErr != nil || checkErr != nil {
		t.Fatalf("a config that loads must still pass: load %v, check %v", loadErr, checkErr)
	}
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

// The regression this pins: every key below is spelled correctly, so the keys
// check is clean and `unarr doctor` reported "All checks passed!" for a file
// `unarr config check` rejected with three problems. Doctor was blind to values.
func TestConfigValuesCheckResultWarnsOnValidKeysWithBadValues(t *testing.T) {
	cfg := loadTestConfig(t, "[downloads]\npreferred_quality = \"4k\"\nseed_ratio = -1.5\n\n[daemon]\nlog_level = \"verbose\"\n")

	if msg, err := configKeysCheckResult(cfg); err != nil || strings.HasPrefix(msg, "!") {
		t.Fatalf("keys are all spelled right; the keys check must stay clean, got %q (err %v)", msg, err)
	}

	msg, err := configValuesCheckResult(cfg)
	// WARN, never FAIL: the loader falls back to its default, so the agent is
	// running. A failure here would give `doctor --quick` a non-zero exit and
	// Docker restarts an unhealthy container — a stray log_level must not become
	// a restart loop.
	if err != nil {
		t.Fatalf("out-of-range values must warn, not fail: %v", err)
	}
	if !strings.HasPrefix(msg, "!") {
		t.Fatalf("expected a WARN-prefixed message, got %q", msg)
	}
	for _, want := range []string{"downloads.preferred_quality", "downloads.seed_ratio", "daemon.log_level"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not name %s: %q", want, msg)
		}
	}
}

func TestConfigValuesCheckResultCleanOnGoodValues(t *testing.T) {
	cfg := loadTestConfig(t, "[downloads]\npreferred_quality = \"1080p\"\nseed_ratio = 1.5\n\n[daemon]\nlog_level = \"debug\"\n")
	msg, err := configValuesCheckResult(cfg)
	if err != nil {
		t.Fatalf("valid values must not fail the check: %v", err)
	}
	if strings.HasPrefix(msg, "!") {
		t.Errorf("valid values must not warn, got %q", msg)
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
