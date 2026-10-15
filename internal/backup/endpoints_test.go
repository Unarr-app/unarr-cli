package backup

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// endpointsOf resolves api_url and mirrors exactly as config.Load does; this
// fails when Load's defaults change and endpointsOf does not follow.
func TestEndpointsOfAgreesWithConfigLoad(t *testing.T) {
	for _, cfg := range []string{
		"",
		"[auth]\napi_key = \"k\"\n",
		"[auth]\napi_url = \"\"\n",
		"[auth]\napi_url = \"  \"\n",
		"[auth]\napi_url = \"https://self.example\"\n",
		"[auth]\nmirrors = []\n",
		"[auth]\nmirrors = [\"https://m.example\"]\n",
	} {
		p := filepath.Join(t.TempDir(), "config.toml")
		write(t, p, cfg)
		want, err := config.Load(p)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := endpointsOf([]byte(cfg))
		if !ok || got.apiURL != want.Auth.APIURL || !slices.Equal(got.mirrors, want.Auth.Mirrors) {
			t.Errorf("config %q: endpointsOf = %+v, config.Load = %q %q", cfg, got, want.Auth.APIURL, want.Auth.Mirrors)
		}
	}
}

// redactedArchiveOf is a secret-free archive of config cfg, as create makes it.
func redactedArchiveOf(t *testing.T, cfg string) *Archive {
	t.Helper()
	l := newLayout(t)
	write(t, l.ConfigPath, cfg)
	res := mustCreate(t, l, CreateOptions{})
	arc, err := Open(res.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	return arc
}

// A secret-free archive keeps this machine's credentials but brings its own
// auth.api_url and auth.mirrors: the plan must say, in the dry-run and before
// --apply writes, that the credentials kept here would go to servers they were
// never sent to. It warns rather than refuses: the daemon takes any URL there.
func TestRedactedRestoreWarnsWhenKeptCredentialsGoSomewhereNew(t *testing.T) {
	evil := strings.Replace(testConfig, `api_url = "https://x.example"`,
		`api_url = "http://evil.example"`+"\nmirrors = [\"https://x.example\", \"http://evil2.example\"]", 1)
	arc := redactedArchiveOf(t, evil)
	l := newLayout(t) // testConfig: api_key set, api_url x.example, default mirrors
	plan, err := arc.Plan(l)
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(plan.Warnings, "\n")
	for _, want := range []string{
		`auth.api_url changes from "https://x.example" to the archive's "http://evil.example"`,
		`auth.mirrors gains "http://evil2.example" from the archive`,
		"api_key/agent_hash kept here",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings lack %q:\n%s", want, w)
		}
	}
	if strings.Contains(w, `gains "https://x.example"`) {
		t.Errorf("a mirror that is this machine's api_url is not new:\n%s", w)
	}

	mustApply(t, arc, l, t.TempDir())
	got := read(t, l.ConfigPath)
	if !strings.Contains(got, `api_key = "SECRET-API-KEY"`) || !strings.Contains(got, "http://evil.example") {
		t.Fatalf("restore is warned about, not refused:\n%s", got)
	}
}

// The local config.toml does not parse, yet the line rewrite still carries its
// api_key over: with nothing to compare the archive's endpoints against, the
// plan used to say nothing at all. It must name where the credentials go — in
// the plan both the dry-run and --apply print — and still restore.
func TestRedactedRestoreWarnsWhenLocalEndpointsAreUnknown(t *testing.T) {
	evil := strings.Replace(testConfig, `"https://x.example"`, `"http://evil.example"`, 1)
	arc := redactedArchiveOf(t, evil)
	l := newLayout(t)
	write(t, l.ConfigPath, "[auth]\napi_key = \"LOCAL-KEY\"\napi_url = \"https://x.example\"\n\n[downloads\ndir = \"x\"\n")
	plan, err := arc.Plan(l)
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(plan.Warnings, "\n")
	for _, want := range []string{
		"this machine's config.toml does not parse",
		`api_key/agent_hash kept here will be sent to the archive's auth.api_url "http://evil.example"`,
		`(mirrors: "https://torrentclaw.to", "https://torrentclaw.com")`,
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings lack %q:\n%s", want, w)
		}
	}

	mustApply(t, arc, l, t.TempDir())
	if got := read(t, l.ConfigPath); !strings.Contains(got, `api_key = "LOCAL-KEY"`) || !strings.Contains(got, "http://evil.example") {
		t.Fatalf("restore is warned about, not refused:\n%s", got)
	}
}

// No warning when nothing goes anywhere new: the same server spelled the way
// config.Load reads it, mirrors only reordered, no credentials kept here, or an
// encrypted archive (its credentials are its own).
func TestRestoreDoesNotWarnWhenCredentialsStayPut(t *testing.T) {
	cases := map[string]struct{ archived, current, pass string }{
		"trailing slash": {
			strings.Replace(testConfig, `"https://x.example"`, `"https://x.example/"`, 1), testConfig, ""},
		"default spelled out vs left out": {
			"[auth]\napi_key = \"k\"\napi_url = \"https://unarr.app\"\n", "[auth]\napi_key = \"LOCAL\"\n", ""},
		"mirrors reordered": {
			"[auth]\napi_key = \"k\"\nmirrors = [\"https://torrentclaw.com\", \"https://torrentclaw.to\"]\n",
			"[auth]\napi_key = \"LOCAL\"\n", ""},
		"no credentials here": {
			"[auth]\napi_url = \"http://evil.example\"\n", "[downloads]\ndir = \"/x\"\n", ""},
		"encrypted archive": {
			strings.Replace(testConfig, `"https://x.example"`, `"http://other.example"`, 1), testConfig, "pw"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLayout(t)
			write(t, l.ConfigPath, tc.archived)
			res := mustCreate(t, l, CreateOptions{Passphrase: tc.pass})
			write(t, l.ConfigPath, tc.current)
			arc, err := Open(res.Path, tc.pass)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := arc.Plan(l)
			if err != nil {
				t.Fatal(err)
			}
			if w := strings.Join(plan.Warnings, "\n"); strings.Contains(w, "auth.api_url") || strings.Contains(w, "auth.mirrors") {
				t.Fatalf("unexpected endpoint warning:\n%s", w)
			}
		})
	}
}
