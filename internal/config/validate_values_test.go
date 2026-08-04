package config

import (
	"strings"
	"testing"
)

// issueKeys flattens findings to their keys for table comparison.
func issueKeys(issues []Issue) []string {
	out := make([]string, 0, len(issues))
	for _, iss := range issues {
		out = append(out, iss.Key)
	}
	return out
}

func TestValueIssuesOnDefaultConfig(t *testing.T) {
	cfg := Default()
	if got := cfg.ValueIssues(); len(got) != 0 {
		t.Errorf("Default() must be clean, got %v", issueKeys(got))
	}
}

// TestValueIssuesOnSavedDefault also covers the empty-string enum fields the
// defaults leave unset (transcode.preset, daemon.downlink, desktop.player):
// "unset" is never a range violation.
func TestValueIssuesOnSavedDefault(t *testing.T) {
	dst := writeConfig(t, "")
	if err := Save(Default(), dst); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cfg, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.ValueIssues(); len(got) != 0 {
		t.Errorf("round-tripped defaults must be clean, got %v", issueKeys(got))
	}
}

func TestValueIssues(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantKey string
		wantSub string // substring the message must carry
	}{
		{
			name:    "port above range",
			mutate:  func(c *Config) { c.Download.StreamPort = 70000 },
			wantKey: "downloads.stream_port",
			wantSub: "0-65535",
		},
		{
			name:    "negative port",
			mutate:  func(c *Config) { c.Download.ListenPort = -1 },
			wantKey: "downloads.listen_port",
		},
		{
			name:    "negative concurrency",
			mutate:  func(c *Config) { c.Download.MaxConcurrent = -3 },
			wantKey: "downloads.max_concurrent",
			wantSub: "zero or positive",
		},
		{
			name:    "unparseable speed",
			mutate:  func(c *Config) { c.Download.MaxDownloadSpeed = "ten megs" },
			wantKey: "downloads.max_download_speed",
			wantSub: "is not a speed",
		},
		{
			name:    "unparseable duration",
			mutate:  func(c *Config) { c.Download.StallTimeout = "30 minutes" },
			wantKey: "downloads.stall_timeout",
			wantSub: "is not a duration",
		},
		{
			name:    "unknown quality",
			mutate:  func(c *Config) { c.Download.PreferredQuality = "4k" },
			wantKey: "downloads.preferred_quality",
			wantSub: "2160p",
		},
		{
			name:    "unknown method in the list",
			mutate:  func(c *Config) { c.Download.PreferredMethods = []string{"debrid", "magnet"} },
			wantKey: "downloads.preferred_methods",
			wantSub: "torrent, debrid, usenet",
		},
		{
			name:    "unknown legacy method",
			mutate:  func(c *Config) { c.Download.PreferredMethod = "nzb" },
			wantKey: "downloads.preferred_method",
		},
		{
			name:    "negative seed ratio",
			mutate:  func(c *Config) { c.Download.SeedRatio = -1 },
			wantKey: "downloads.seed_ratio",
		},
		{
			name:    "unknown hw accel",
			mutate:  func(c *Config) { c.Download.Transcode.HWAccel = "cuda" },
			wantKey: "downloads.transcode.hw_accel",
			wantSub: "nvenc",
		},
		{
			name:    "unknown encoder preset",
			mutate:  func(c *Config) { c.Download.Transcode.Preset = "turbo" },
			wantKey: "downloads.transcode.preset",
		},
		{
			name:    "negative hls cache budget",
			mutate:  func(c *Config) { c.Download.HLSCache.SizeGB = -5 },
			wantKey: "downloads.hls_cache.size_gb",
		},
		{
			name:    "bad scan interval",
			mutate:  func(c *Config) { c.Library.ScanInterval = "hourly" },
			wantKey: "library.scan_interval",
		},
		{
			name:    "bad trickplay interval",
			mutate:  func(c *Config) { c.Library.Trickplay.Interval = "ten seconds" },
			wantKey: "library.trickplay.interval",
		},
		{
			name:    "negative load ratio",
			mutate:  func(c *Config) { c.Library.PrewarmMaxLoadRatio = -0.5 },
			wantKey: "library.prewarm_max_load_ratio",
		},
		{
			name:    "api url without scheme",
			mutate:  func(c *Config) { c.Auth.APIURL = "unarr.app" },
			wantKey: "auth.api_url",
			wantSub: "http(s) URL",
		},
		{
			name:    "mirror without scheme",
			mutate:  func(c *Config) { c.Auth.Mirrors = []string{"https://ok.example", "ftp://bad.example"} },
			wantKey: "auth.mirrors",
		},
		{
			name:    "unknown downlink",
			mutate:  func(c *Config) { c.Daemon.Downlink = "websocket" },
			wantKey: "daemon.downlink",
		},
		{
			name:    "unknown desktop player",
			mutate:  func(c *Config) { c.Desktop.Player = "web" },
			wantKey: "desktop.player",
			wantSub: "mpv",
		},
		{
			name:    "bad status interval",
			mutate:  func(c *Config) { c.Daemon.StatusInterval = "often" },
			wantKey: "daemon.status_interval",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)
			issues := cfg.ValueIssues()
			if len(issues) != 1 {
				t.Fatalf("want exactly one issue, got %v", issueKeys(issues))
			}
			if issues[0].Key != tt.wantKey {
				t.Fatalf("issue key = %q, want %q", issues[0].Key, tt.wantKey)
			}
			if tt.wantSub != "" && !strings.Contains(issues[0].Message, tt.wantSub) {
				t.Errorf("message %q missing %q", issues[0].Message, tt.wantSub)
			}
		})
	}
}

// TestValueIssuesTolerateSelfHealingValues pins the deliberate omissions: keys
// the loader documents a fallback for must not be reported.
func TestValueIssuesTolerateSelfHealingValues(t *testing.T) {
	cfg := Default()
	cfg.Download.MaxStreamSessions = 0         // documented: <= 0 → 1
	cfg.Library.Cleanup.MinVideoBytes = "1MiB" // documented: unparseable → 1 MiB
	cfg.Download.SeedTime = "0"                // "0" = no time target
	cfg.Download.MetadataTimeout = "0"
	cfg.Download.MaxUploadSpeed = "0"
	cfg.Download.PreferredMethods = []string{"Usenet", " debrid "} // normalised by MethodOrder
	if got := cfg.ValueIssues(); len(got) != 0 {
		t.Errorf("self-healing values must not be reported, got %v", issueKeys(got))
	}
}
