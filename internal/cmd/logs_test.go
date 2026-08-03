package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/logging"
)

// withConfig installs a config for the duration of a test, bypassing the lazy
// loader's memoisation and restoring it afterwards.
func withConfig(t *testing.T, cfg config.Config) {
	t.Helper()
	prevCfg, prevLoaded, prevFlag := appCfg, cfgLoaded, logLevelFlag
	t.Cleanup(func() { appCfg, cfgLoaded, logLevelFlag = prevCfg, prevLoaded, prevFlag })
	appCfg, cfgLoaded = cfg, true
}

// withJSONFlag sets the global --json for the duration of a test.
func withJSONFlag(t *testing.T, v bool) {
	t.Helper()
	prev := jsonOut
	t.Cleanup(func() { jsonOut = prev })
	jsonOut = v
}

func TestResolveLogLevelPrecedence(t *testing.T) {
	cfg := config.Default()
	cfg.Daemon.LogLevel = "warn"
	withConfig(t, cfg)

	// Config only.
	got, err := resolveLogLevel(cfg, "")
	if err != nil || got != logging.LevelWarn {
		t.Fatalf("config level ignored: %v, %v", got, err)
	}

	// The global --log-level beats the config.
	logLevelFlag = "error"
	got, err = resolveLogLevel(cfg, "")
	if err != nil || got != logging.LevelError {
		t.Fatalf("--log-level did not override the config: %v, %v", got, err)
	}

	// The command's own --level beats both.
	got, err = resolveLogLevel(cfg, "debug")
	if err != nil || got != logging.LevelDebug {
		t.Fatalf("--level did not win: %v, %v", got, err)
	}
}

func TestResolveLogLevelFallsBackToTheBuiltInDefault(t *testing.T) {
	cfg := config.Default()
	cfg.Daemon.LogLevel = ""
	withConfig(t, cfg)

	got, err := resolveLogLevel(cfg, "")
	if err != nil || got != logging.DefaultLevel {
		t.Fatalf("got %v (%v), want the built-in default", got, err)
	}
}

func TestResolveLogLevelRejectsATypo(t *testing.T) {
	cfg := config.Default()
	withConfig(t, cfg)
	if _, err := resolveLogLevel(cfg, "warm"); err == nil {
		t.Fatal("a misspelled --level must fail loudly")
	}
}

func TestBuildLogQueryCarriesTheFlagsAndTheConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Daemon.LogFormat = "json"
	cfg.Daemon.LogMaxFiles = 5
	withConfig(t, cfg)

	q, err := buildLogQuery(logsOptions{lines: 12, level: "warn", grep: "nzb", since: "2h"})
	if err != nil {
		t.Fatalf("buildLogQuery: %v", err)
	}
	if q.Lines != 12 || q.Grep != "nzb" || q.MinLevel != logging.LevelWarn {
		t.Fatalf("flags not carried: %+v", q)
	}
	if q.Format != logging.FormatJSON || q.MaxFiles != 5 {
		t.Fatalf("config not carried: %+v", q)
	}
	if q.Since.IsZero() {
		t.Fatal("--since 2h produced no cut-off")
	}
	if filepath.Base(q.Path) != logFileName {
		t.Fatalf("query points at %q, want the daemon log", q.Path)
	}
}

func TestBuildLogQueryRejectsABadSince(t *testing.T) {
	withConfig(t, config.Default())
	if _, err := buildLogQuery(logsOptions{since: "yesterday"}); err == nil {
		t.Fatal("an unreadable --since must be reported before any file is opened")
	}
}

func TestBuildLogQueryRejectsABadConfiguredFormat(t *testing.T) {
	cfg := config.Default()
	cfg.Daemon.LogFormat = "yaml"
	withConfig(t, cfg)
	if _, err := buildLogQuery(logsOptions{}); err == nil {
		t.Fatal("an unsupported [daemon] log_format must be reported")
	}
}

func TestResolveLogFormatPrecedence(t *testing.T) {
	cfg := config.Default()
	withConfig(t, cfg)

	// Nothing set anywhere: the built-in default.
	cfg.Daemon.LogFormat = ""
	withJSONFlag(t, false)
	if got, err := resolveLogFormat(cfg); err != nil || got != logging.DefaultFormat {
		t.Fatalf("got %v (%v), want the built-in default", got, err)
	}

	// Config only.
	cfg.Daemon.LogFormat = "json"
	if got, err := resolveLogFormat(cfg); err != nil || got != logging.FormatJSON {
		t.Fatalf("got %v (%v), want the configured format", got, err)
	}

	// The global --json beats a config that says text.
	cfg.Daemon.LogFormat = "text"
	jsonOut = true
	if got, err := resolveLogFormat(cfg); err != nil || got != logging.FormatJSON {
		t.Fatalf("got %v (%v), want --json to override [daemon] log_format", got, err)
	}

	// A broken config value is still reported, even when --json makes it moot.
	cfg.Daemon.LogFormat = "yaml"
	if _, err := resolveLogFormat(cfg); err == nil {
		t.Fatal("an unsupported [daemon] log_format must be reported even under --json")
	}
}

func TestBuildLogQueryHonoursTheGlobalJSONFlag(t *testing.T) {
	// Regression: --json parsed fine but did nothing, so `unarr logs --json | jq`
	// got plain text.
	cfg := config.Default()
	cfg.Daemon.LogFormat = "text"
	withConfig(t, cfg)
	withJSONFlag(t, true)

	q, err := buildLogQuery(logsOptions{})
	if err != nil {
		t.Fatalf("buildLogQuery: %v", err)
	}
	if q.Format != logging.FormatJSON {
		t.Fatalf("got %v, want --json to force json-lines", q.Format)
	}
}

func TestBuildLogQueryRejectsABadGrepBeforeAnythingRuns(t *testing.T) {
	// The filters compile the pattern lazily; on a systemd box journalctl is
	// already spawned by then. Catching it here is what keeps that from hanging.
	withConfig(t, config.Default())
	withJSONFlag(t, false)
	_, err := buildLogQuery(logsOptions{grep: "["})
	if err == nil {
		t.Fatal("an uncompilable --grep must be reported before any reader starts")
	}
	if !strings.Contains(err.Error(), "--grep") {
		t.Fatalf("error %q does not name the offending flag", err)
	}
}

func TestBuildLogQueryRejectsANegativeLineCount(t *testing.T) {
	withConfig(t, config.Default())
	if _, err := buildLogQuery(logsOptions{lines: -5}); err == nil {
		t.Fatal("a negative -n must fail with a readable error")
	}
}

func TestRunLogsFailsFastOnABadGrepInsteadOfHanging(t *testing.T) {
	withConfig(t, config.Default())
	withJSONFlag(t, false)

	done := make(chan error, 1)
	go func() { done <- runLogs(logsOptions{follow: true, grep: "("}) }()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "--grep") {
			t.Fatalf("got %v, want the pattern error", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("`unarr logs -f --grep '('` hung instead of reporting the bad pattern")
	}
}

func TestLogRingExistsSeesARotatedCopyAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, logFileName)
	q := logging.Query{Path: path, MaxFiles: 3}

	if logRingExists(q) {
		t.Fatal("an empty data dir has no log ring")
	}
	// Right after a rename-based rotation the live file can be momentarily
	// absent while all the history sits in .1 — that still counts.
	if err := os.WriteFile(logging.RotatedPath(path, 1), []byte("old\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !logRingExists(q) {
		t.Fatal("a rotated copy on its own must still count as a log ring")
	}
}

func TestLogRingOptionsMirrorTheConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Daemon.LogMaxSizeMB = 7
	cfg.Daemon.LogMaxFiles = 2
	withConfig(t, cfg)

	opts := logRingOptions("/tmp/unarr.log")
	if opts.MaxSizeMB != 7 || opts.MaxFiles != 2 || opts.Path != "/tmp/unarr.log" {
		t.Fatalf("got %+v, want the configured ring", opts)
	}
}

func TestLogsAliasSharesTheFlagSet(t *testing.T) {
	// `unarr daemon logs` is documented as an alias; if its flags drifted from
	// `unarr logs`, an existing script would break on the next release.
	top := newLogsCmd()
	alias := newDaemonLogsCmd()
	for _, name := range []string{"follow", "lines", "since", "level", "grep", "boot"} {
		if top.Flags().Lookup(name) == nil {
			t.Fatalf("unarr logs is missing --%s", name)
		}
		if alias.Flags().Lookup(name) == nil {
			t.Fatalf("unarr daemon logs is missing --%s", name)
		}
	}
	if alias.Flags().ShorthandLookup("f") == nil || alias.Flags().ShorthandLookup("n") == nil {
		t.Fatal("the alias must keep its historical -f / -n shorthands")
	}
}

func TestLogsHasARotateSubcommand(t *testing.T) {
	var found bool
	for _, c := range newLogsCmd().Commands() {
		if c.Name() == "rotate" {
			found = true
		}
	}
	if !found {
		t.Fatal("`unarr logs rotate` is the manual trim; it must be registered")
	}
}
