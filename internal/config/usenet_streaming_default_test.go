package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Angle 1 — Load decision table: every combination of the two keys plus the
// shapes of file that can exist in the wild.
func TestUsenetStreamingLoadMatrix(t *testing.T) {
	cases := []struct {
		name       string
		body       string // "" with absent=true means no file at all
		absent     bool
		wantStream bool
		// wantMarker is the in-memory marker (what the next Save writes);
		// wantPending is "the file on disk lacks it" (what the daemon persists).
		wantMarker  bool
		wantPending bool
	}{
		{name: "no file", absent: true, wantStream: true, wantMarker: true},
		{name: "empty file", body: "", wantStream: true, wantMarker: true, wantPending: true},
		{name: "no downloads section", body: "[auth]\napi_key = \"k\"\n", wantStream: true, wantMarker: true, wantPending: true},
		{name: "downloads without the key", body: "[downloads]\nmax_concurrent = 5\n", wantStream: true, wantMarker: true, wantPending: true},
		{name: "legacy false, no marker", body: "[downloads]\nusenet_streaming = false\n", wantStream: true, wantMarker: true, wantPending: true},
		{name: "legacy true, no marker", body: "[downloads]\nusenet_streaming = true\n", wantStream: true, wantMarker: true, wantPending: true},
		{name: "false + marker true = opt-out", body: "[downloads]\nusenet_streaming = false\nusenet_streaming_default_applied = true\n", wantStream: false, wantMarker: true},
		{name: "true + marker true", body: "[downloads]\nusenet_streaming = true\nusenet_streaming_default_applied = true\n", wantStream: true, wantMarker: true},
		{name: "marker only", body: "[downloads]\nusenet_streaming_default_applied = true\n", wantStream: true, wantMarker: true},
		{name: "false + hand-written marker false", body: "[downloads]\nusenet_streaming = false\nusenet_streaming_default_applied = false\n", wantStream: false, wantMarker: false},
		{name: "legacy false among other keys", body: "[auth]\napi_key = \"k\"\n[downloads]\nmax_concurrent = 1\nusenet_streaming = false\nstream_port = 12000\n", wantStream: true, wantMarker: true, wantPending: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "absent.toml")
			if !tc.absent {
				path = writeCfg(t, tc.body)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Download.UsenetStreaming; got != tc.wantStream {
				t.Errorf("UsenetStreaming = %v, want %v", got, tc.wantStream)
			}
			if got := cfg.Download.UsenetStreamingDefaultApplied; got != tc.wantMarker {
				t.Errorf("marker = %v, want %v", got, tc.wantMarker)
			}
			if got := cfg.UsenetMarkerPending(); got != tc.wantPending {
				t.Errorf("UsenetMarkerPending = %v, want %v", got, tc.wantPending)
			}
		})
	}
}

// Angle 2 — persistence: after one Save the migration never fires again and the
// value is a fixpoint under Load→Save→Load (no flip-flop, no drift).
func TestUsenetStreamingSaveLoadFixpoint(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("user choice %v", stream), func(t *testing.T) {
			legacy := writeCfg(t, "[downloads]\nusenet_streaming = false\n")
			cfg, err := Load(legacy)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Download.UsenetStreaming = stream // the choice made AFTER the migration
			for i := 0; i < 3; i++ {
				if err := Save(cfg, legacy); err != nil {
					t.Fatal(err)
				}
				if cfg, err = Load(legacy); err != nil {
					t.Fatal(err)
				}
				if cfg.Download.UsenetStreaming != stream {
					t.Fatalf("round %d: UsenetStreaming = %v, want %v", i, cfg.Download.UsenetStreaming, stream)
				}
			}
		})
	}
}

// Angle 3 — a brand-new install: Default() must already carry the marker, so the
// first Save writes both keys and a later opt-out survives.
func TestUsenetStreamingFreshInstallPersistsMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.toml")
	if err := Save(Default(), path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, want := range []string{"usenet_streaming = true", "usenet_streaming_default_applied = true"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("fresh config missing %q:\n%s", want, raw)
		}
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Download.UsenetStreaming = false
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(path); err != nil || cfg.Download.UsenetStreaming {
		t.Fatalf("opt-out lost on a fresh install: err=%v streaming=%v", err, cfg.Download.UsenetStreaming)
	}
}

// Angle 4 — schema hygiene: the marker is a known key (no "unknown key" warning
// in doctor / `config check`) and the migration touches nothing else.
func TestUsenetStreamingMigrationIsIsolated(t *testing.T) {
	body := "[auth]\napi_key = \"k\"\n[downloads]\nmax_concurrent = 7\nstream_port = 12345\nrequire_stream_token = false\nusenet_streaming = false\n"
	migrated, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if k := migrated.UnknownKeys(); len(k) != 0 {
		t.Errorf("unexpected unknown keys after migration: %v", k)
	}
	withMarker, err := Load(writeCfg(t, body+"usenet_streaming_default_applied = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if k := withMarker.UnknownKeys(); len(k) != 0 {
		t.Errorf("marker reported as unknown key: %v", k)
	}
	// Differences between the two must be exactly the two usenet fields.
	a, b := migrated.Download, withMarker.Download
	a.UsenetStreaming, b.UsenetStreaming = false, false
	a.UsenetStreamingDefaultApplied, b.UsenetStreamingDefaultApplied = false, false
	if !reflect.DeepEqual(a, b) {
		t.Errorf("migration changed unrelated download fields:\n%+v\n%+v", a, b)
	}
	if migrated.Download.MaxConcurrent != 7 || migrated.Download.StreamPort != 12345 || migrated.Download.RequireStreamToken {
		t.Errorf("explicit user values were overwritten: %+v", migrated.Download)
	}
}

// Angle 5 — robustness: hostile / odd values must not panic or silently opt out.
func TestUsenetStreamingOddInputs(t *testing.T) {
	// Wrong type → parse error, surfaced (never swallowed into "off").
	if _, err := Load(writeCfg(t, "[downloads]\nusenet_streaming = \"yes\"\n")); err == nil {
		t.Error("string value for a bool must be a parse error")
	}
	// Dotted-key / inline-table spellings of the same keys behave like the table form.
	for _, body := range []string{
		"downloads.usenet_streaming = false\n",
		"downloads = { usenet_streaming = false }\n",
	} {
		cfg, err := Load(writeCfg(t, body))
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if !cfg.Download.UsenetStreaming {
			t.Errorf("%q: legacy false not migrated", body)
		}
	}
	// CRLF files (Windows editors).
	cfg, err := Load(writeCfg(t, "[downloads]\r\nusenet_streaming = false\r\nusenet_streaming_default_applied = true\r\n"))
	if err != nil || cfg.Download.UsenetStreaming {
		t.Errorf("CRLF opt-out not honoured: err=%v streaming=%v", err, cfg.Download.UsenetStreaming)
	}
}

// Angle 6 — concurrency: Load has no shared state, so parallel loads (daemon +
// CLI + desktop reading the same file) agree. Meaningful under -race.
func TestUsenetStreamingConcurrentLoad(t *testing.T) {
	path := writeCfg(t, "[downloads]\nusenet_streaming = false\n")
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg, err := Load(path)
			if err != nil || !cfg.Download.UsenetStreaming {
				t.Errorf("concurrent Load: err=%v streaming=%v", err, cfg.Download.UsenetStreaming)
			}
		}()
	}
	wg.Wait()
}

// Angle 7 — fuzz: Load never panics and the migration invariant holds for any
// input that parses: no marker key in the file ⇒ streaming is ON and marker set.
func FuzzLoadUsenetStreaming(f *testing.F) {
	for _, s := range []string{
		"", "[downloads]\nusenet_streaming = false\n", "[downloads]\nusenet_streaming = true\n",
		"[downloads]\nusenet_streaming = false\nusenet_streaming_default_applied = true\n",
		"downloads.usenet_streaming = false\n", "[auth]\napi_key = \"x\"\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, body string) {
		p := filepath.Join(t.TempDir(), "c.toml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Skip()
		}
		cfg, err := Load(p)
		if err != nil {
			return // unparseable input is allowed to error, never to panic
		}
		// Decide "has the marker KEY" from the parsed document, not the raw text —
		// a comment or string containing the word must not disable the check.
		var doc struct {
			Downloads map[string]any `toml:"downloads"`
		}
		if _, derr := toml.Decode(body, &doc); derr != nil {
			return
		}
		if _, has := doc.Downloads["usenet_streaming_default_applied"]; !has {
			if !cfg.Download.UsenetStreaming || !cfg.Download.UsenetStreamingDefaultApplied || !cfg.UsenetMarkerPending() {
				t.Fatalf("no marker key in file but not migrated: %+v\n%q", cfg.Download, body)
			}
		}
	})
}
