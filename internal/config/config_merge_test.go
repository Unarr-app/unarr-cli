package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const mergeFixture = `# my unarr config — hand edited
[auth]
# keep me
api_key = "secret" # inline comment
future_option = "from a newer CLI"

[downloads]
dir = '/srv/media'   # literal string, my style
max_concurrent = 3

[mystery]
answer = 42
`

func TestSaveMerge(t *testing.T) {
	cases := []struct {
		name     string
		orig     string // "" = no file
		mutate   func(*Config)
		contains []string
		absent   []string
		check    func(t *testing.T, got Config)
	}{
		{
			name:     "comments and unknown keys survive a value change",
			orig:     mergeFixture,
			mutate:   func(c *Config) { c.Download.MaxConcurrent = 7 },
			contains: []string{"# my unarr config — hand edited", "# keep me", `api_key = "secret" # inline comment`, `future_option = "from a newer CLI"`, "[mystery]\nanswer = 42", "max_concurrent = 7", "dir = '/srv/media'   # literal string, my style"},
			absent:   []string{"max_concurrent = 3"},
		},
		{
			name:     "changed value keeps its trailing comment",
			orig:     mergeFixture,
			mutate:   func(c *Config) { c.Download.Dir = "/data" },
			contains: []string{`dir = "/data"   # literal string, my style`},
		},
		{
			name:     "new section appended",
			orig:     "[auth]\napi_key = \"k\"\n",
			mutate:   func(c *Config) { c.Desktop.Player = "mpv" },
			contains: []string{"[auth]\napi_key = \"k\"\n", "[desktop]", `player = "mpv"`},
		},
		{
			name:   "no file writes the plain encoding",
			mutate: func(c *Config) { c.Auth.APIKey = "k" },
			check: func(t *testing.T, got Config) {
				if got.Auth.APIKey != "k" {
					t.Fatalf("api key = %q", got.Auth.APIKey)
				}
			},
		},
		{
			name:     "windows path round-trips",
			orig:     mergeFixture,
			mutate:   func(c *Config) { c.Download.Dir = `D:\Downloads` },
			contains: []string{`dir = "D:\\Downloads"`},
			check:    wantDir(`D:\Downloads`),
		},
		{
			name:   "windows path with chinese round-trips",
			orig:   mergeFixture,
			mutate: func(c *Config) { c.Download.Dir = `D:\下载` },
			check:  wantDir(`D:\下载`),
		},
		{
			name:     "cleared omitempty key is removed, not resurrected",
			orig:     "[desktop]\n# player\nplayer = \"vlc\"\n",
			mutate:   func(c *Config) { c.Desktop.Player = "" },
			contains: []string{"# player"},
			absent:   []string{"vlc"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if tc.orig != "" {
				if err := os.WriteFile(path, []byte(tc.orig), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(&cfg)
			if err := Save(cfg, path); err != nil {
				t.Fatalf("Save: %v", err)
			}
			raw, _ := os.ReadFile(path)
			for _, s := range tc.contains {
				if !strings.Contains(string(raw), s) {
					t.Errorf("missing %q in:\n%s", s, raw)
				}
			}
			for _, s := range tc.absent {
				if strings.Contains(string(raw), s) {
					t.Errorf("unexpected %q in:\n%s", s, raw)
				}
			}
			got, err := Load(path)
			if err != nil {
				t.Fatalf("reload: %v\n%s", err, raw)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
			assertSameLoad(t, cfg, got)
		})
	}
}

func wantDir(dir string) func(*testing.T, Config) {
	return func(t *testing.T, got Config) {
		if got.Download.Dir != dir {
			t.Fatalf("dir = %q, want %q", got.Download.Dir, dir)
		}
	}
}

// assertSameLoad compares what both configs would write, ignoring unexported diagnostics.
func assertSameLoad(t *testing.T, want, got Config) {
	t.Helper()
	w, g := want, got
	w.unknownKeys, g.unknownKeys = nil, nil
	w.usenetMarkerPending, g.usenetMarkerPending = false, false
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.toml"), filepath.Join(dir, "b.toml")
	if Save(w, a) != nil || Save(g, b) != nil {
		t.Fatal("save for comparison failed")
	}
	ra, _ := os.ReadFile(a)
	rb, _ := os.ReadFile(b)
	if string(ra) != string(rb) {
		t.Fatalf("reloaded config differs:\nwant:\n%s\ngot:\n%s", ra, rb)
	}
}

func TestSaveRoundTripIsByteIdentical(t *testing.T) {
	for name, orig := range map[string]string{
		"fixture":      mergeFixture,
		"crlf":         strings.ReplaceAll(mergeFixture, "\n", "\r\n"),
		"literal path": "[downloads]\ndir = 'D:\\下载'\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			// First Save fills in the keys Load defaults; the second must be a no-op.
			if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, _ := Load(path)
			if err := Save(cfg, path); err != nil {
				t.Fatal(err)
			}
			first, _ := os.ReadFile(path)
			if !strings.HasPrefix(string(first), strings.SplitN(orig, "[", 2)[0]) {
				t.Errorf("header comment lost:\n%s", first)
			}
			cfg, _ = Load(path)
			if err := Save(cfg, path); err != nil {
				t.Fatal(err)
			}
			second, _ := os.ReadFile(path)
			if string(first) != string(second) {
				t.Fatalf("Load→Save changed the file:\n--- first\n%s\n--- second\n%s", first, second)
			}
		})
	}
}

func TestSavePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	for range 2 { // create, then merge into the existing file
		if err := Save(Default(), path); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v", st.Mode().Perm())
		}
	}
}

func TestMergeConfigTextFallbackKeepsUnknownKeys(t *testing.T) {
	// Dotted keys defining [downloads.transcode] inline defeat the line patch
	// (the new key needs a table header that would redefine it), so the merge
	// must fall back to a re-encode that still carries the unknown key.
	orig := "[downloads]\ntranscode.enabled = false\nzzz_unknown = \"keep\"\n"
	cfg, err := loadString(t, orig)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Download.Transcode.Preset = "fast"
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "transcode.enabled") {
		t.Fatalf("expected the re-encode fallback, got a patched file:\n%s", raw)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Download.Transcode.Preset != "fast" || got.Download.Transcode.Enabled {
		t.Fatalf("values lost: %+v", got.Download.Transcode)
	}
	if keys := got.UnknownKeys(); len(keys) != 1 || keys[0] != "downloads.zzz_unknown" {
		t.Fatalf("unknown keys = %v", keys)
	}
}

func loadString(t *testing.T, s string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.toml")
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestScanValueSpans(t *testing.T) {
	cases := map[string]string{
		`a = "x # y" # c`:          `"x # y"`,
		`a = 'C:\dir\' # c`:        `'C:\dir\'`,
		`a = ["x", "]"] `:          `["x", "]"]`,
		"a = [\n  1,\n  2, # c\n]": "[\n  1,\n  2, # c\n]",
		"a = \"\"\"l1\nl2\"\"\"":   "\"\"\"l1\nl2\"\"\"",
		`a = { b = "}" }`:          `{ b = "}" }`,
		`"quoted.key" = 1`:         `1`,
	}
	for in, want := range cases {
		d, ok := scanDoc(in)
		if !ok || len(d.order) != 1 {
			t.Errorf("%q: scan failed", in)
			continue
		}
		if got := d.valueText(d.entries[d.order[0]]); got != want {
			t.Errorf("%q: value %q, want %q", in, got, want)
		}
	}
}
