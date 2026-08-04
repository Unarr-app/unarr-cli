package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeConfig drops a TOML body in a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestValidKeysDerivedFromTags(t *testing.T) {
	keys := ValidKeys()

	// Leaf keys, nested tables, the table entries themselves, and a section
	// whose tag carries ",omitempty" (the option must be stripped).
	for _, want := range []string{
		"auth.api_key", "auth.mirrors",
		"downloads", "downloads.dir", "downloads.min_free_disk_mb",
		"downloads.transcode", "downloads.transcode.hw_accel",
		"downloads.vpn.required", "downloads.hls_cache.size_gb",
		"library.trickplay.interval", "library.cleanup.dedup_exact",
		"daemon.auto_upgrade", // *bool — pointers must not stop the walk
		"telemetry.enabled",
		"desktop.player", "desktop.player_command",
		"organize.tv_shows_dir", "general.no_color",
	} {
		if !contains(keys, want) {
			t.Errorf("ValidKeys() missing %q", want)
		}
	}

	// The diagnostic field is unexported and must never look like a valid key.
	for _, k := range keys {
		if strings.Contains(k, "unknownKeys") {
			t.Errorf("ValidKeys() leaked unexported field: %q", k)
		}
	}
}

func TestLoadReportsUnknownKeys(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		wantKeys       []string
		wantSuggestion map[string]string
	}{
		{
			name: "clean config has no unknown keys",
			body: "[auth]\napi_key = \"k\"\n\n[downloads]\ndir = \"/data\"\nmin_free_disk_mb = 1024\n",
		},
		{
			name:           "typo in a leaf key",
			body:           "[downloads]\nmin_free_disk_gb = 5\n",
			wantKeys:       []string{"downloads.min_free_disk_gb"},
			wantSuggestion: map[string]string{"downloads.min_free_disk_gb": "downloads.min_free_disk_mb"},
		},
		{
			name:           "typo in the section name",
			body:           "[download]\ndir = \"/data\"\n",
			wantKeys:       []string{"download"},
			wantSuggestion: map[string]string{"download": "downloads"},
		},
		{
			name:           "right key under the wrong section",
			body:           "[general]\nmax_concurrent = 4\n",
			wantKeys:       []string{"general.max_concurrent"},
			wantSuggestion: map[string]string{"general.max_concurrent": "downloads.max_concurrent"},
		},
		{
			name:           "typo in a nested table key",
			body:           "[downloads.transcode]\nhw_accell = \"auto\"\n",
			wantKeys:       []string{"downloads.transcode.hw_accell"},
			wantSuggestion: map[string]string{"downloads.transcode.hw_accell": "downloads.transcode.hw_accel"},
		},
		{
			name:           "unrelated key gets no absurd suggestion",
			body:           "[downloads]\nsomething_entirely_made_up = 1\n",
			wantKeys:       []string{"downloads.something_entirely_made_up"},
			wantSuggestion: map[string]string{"downloads.something_entirely_made_up": ""},
		},
		{
			name:     "unknown table is reported once, not per leaf",
			body:     "[nosuchsection]\na = 1\nb = 2\n\n[nosuchsection.deep]\nc = 3\n",
			wantKeys: []string{"nosuchsection"},
		},
		{
			name:     "several typos are all reported",
			body:     "[auth]\napi_ky = \"k\"\n\n[library]\nworkerz = 4\n",
			wantKeys: []string{"auth.api_ky", "library.workerz"},
			wantSuggestion: map[string]string{
				"auth.api_ky":     "auth.api_key",
				"library.workerz": "library.workers",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tt.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			issues := cfg.UnknownKeyIssues()
			got := make([]string, 0, len(issues))
			for _, iss := range issues {
				got = append(got, iss.Key)
			}
			if !reflect.DeepEqual(got, tt.wantKeys) && !(len(got) == 0 && len(tt.wantKeys) == 0) {
				t.Fatalf("unknown keys = %v, want %v", got, tt.wantKeys)
			}

			for _, iss := range issues {
				want, ok := tt.wantSuggestion[iss.Key]
				if !ok {
					continue
				}
				if iss.Suggestion != want {
					t.Errorf("suggestion for %q = %q, want %q", iss.Key, iss.Suggestion, want)
				}
				if want != "" && !strings.Contains(iss.Message, "did you mean "+strconvQuote(want)) {
					t.Errorf("message %q does not offer %q", iss.Message, want)
				}
			}
		})
	}
}

// strconvQuote is a tiny local helper so the message assertion above reads the
// same way the Issue builds it (%q).
func strconvQuote(s string) string { return "\"" + s + "\"" }

func TestUnknownKeysEmptyForDefaultAndMissingFile(t *testing.T) {
	def := Default()
	if got := def.UnknownKeys(); len(got) != 0 {
		t.Errorf("Default().UnknownKeys() = %v, want none", got)
	}
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.UnknownKeys(); len(got) != 0 {
		t.Errorf("missing file UnknownKeys() = %v, want none", got)
	}
}

// TestSaveOmitsDiagnostics is the round-trip trap: the unknown-key list rides on
// Config but must never be written back, and a Load→Save→Load cycle must not
// invent or drop a section (the `omitempty` [desktop] / [telemetry] contract).
func TestSaveLoadRoundTripKeepsSections(t *testing.T) {
	src := writeConfig(t, `
[auth]
api_key = "secret"
api_url = "https://unarr.app"

[downloads]
dir = "/data/downloads"
min_free_disk_gb = 5

[library]
workers = 4
`)
	cfg, err := Load(src)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.UnknownKeys()) != 1 {
		t.Fatalf("expected the typo to be recorded, got %v", cfg.UnknownKeys())
	}

	dst := filepath.Join(t.TempDir(), "out.toml")
	if err := Save(cfg, dst); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	written := string(raw)

	// The diagnostic must not leak into the file, in any spelling.
	for _, forbidden := range []string{"unknownKeys", "unknown_keys", "min_free_disk_gb"} {
		if strings.Contains(written, forbidden) {
			t.Errorf("Save() wrote %q into the config:\n%s", forbidden, written)
		}
	}
	// [desktop] was never configured — encoding must not invent it (omitempty).
	if strings.Contains(written, "[desktop]") {
		t.Errorf("Save() invented a [desktop] section:\n%s", written)
	}

	// Re-loading the saved file yields the same settings and a clean bill of
	// health (the typo is gone because it was never re-serialised).
	back, err := Load(dst)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := back.UnknownKeys(); len(got) != 0 {
		t.Errorf("reloaded config reports unknown keys %v", got)
	}
	cfg.unknownKeys = nil // the only field that legitimately differs
	if !reflect.DeepEqual(cfg, back) {
		t.Errorf("round-trip changed the config:\n got %+v\nwant %+v", back, cfg)
	}
}

// TestSaveLoadRoundTripKeepsDesktop guards the other half of the [desktop]
// trap: a section the user DID set must survive a daemon-side round-trip.
func TestSaveLoadRoundTripKeepsDesktop(t *testing.T) {
	src := writeConfig(t, "[desktop]\nplayer = \"mpv\"\n")
	cfg, err := Load(src)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "out.toml")
	if err := Save(cfg, dst); err != nil {
		t.Fatalf("Save: %v", err)
	}
	back, err := Load(dst)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if back.Desktop.Player != "mpv" {
		t.Errorf("round-trip lost desktop.player: %+v", back.Desktop)
	}
}

func TestSuggestKeyThresholds(t *testing.T) {
	valid := []string{
		"downloads.dir", "downloads.min_free_disk_mb", "downloads.max_concurrent",
		"downloads.transcode.max_concurrent", "library.workers", "general.locale",
	}
	tests := []struct {
		unknown string
		want    string
	}{
		{"downloads.min_free_disk_gb", "downloads.min_free_disk_mb"},
		{"downloads.dirr", "downloads.dir"},
		{"library.workers", ""},                                // actually valid — never suggest itself
		{"downloads.zzz", ""},                                  // nothing close
		{"general.max_concurrent", "downloads.max_concurrent"}, // wrong section, closest parent wins
		{"downloads.wrkers", ""},                               // leaf typo in the wrong section: too far to guess
	}
	for _, tt := range tests {
		if got := SuggestKey(tt.unknown, valid); got != tt.want {
			t.Errorf("SuggestKey(%q) = %q, want %q", tt.unknown, got, tt.want)
		}
	}
}

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "abc", 3},
		{"abc", "", 3},
		{"abc", "abc", 0},
		{"kitten", "sitting", 3},
		{"min_free_disk_gb", "min_free_disk_mb", 1},
		{"añó", "ano", 2}, // rune-based, not byte-based
	}
	for _, tt := range tests {
		if got := levenshtein(tt.a, tt.b); got != tt.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestTopLevelUnknownCollapsesNestedKeys(t *testing.T) {
	in := []string{"nosuchsection.b.c", "nosuchsection", "auth.typo", "nosuchsection.a"}
	want := []string{"auth.typo", "nosuchsection"}
	if got := topLevelUnknown(in); !reflect.DeepEqual(got, want) {
		t.Errorf("topLevelUnknown(%v) = %v, want %v", in, got, want)
	}
}
