package support

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// scrubberWithHome builds a Scrubber whose only rewrite is the given home, so
// the tests do not depend on the machine they run on. homeRewrites() reads the
// real HOME, which is exactly what makes it untestable directly.
func scrubberWithHome(t *testing.T, home string) *Scrubber {
	t.Helper()
	s := NewScrubber(config.Default())
	s.rewrites = []rewrite{{re: homePattern(home), to: homeMarker}}
	return s
}

func TestScrubRewritesHomePaths(t *testing.T) {
	s := scrubberWithHome(t, "/home/anna")
	cases := []struct{ in, want string }{
		{"/home/anna/Media/movies", "~/Media/movies"},
		{"config: /home/anna/.config/unarr/config.toml", "config: ~/.config/unarr/config.toml"},
		// A bare home with nothing after it, the shape an env dump takes.
		{"HOME=/home/anna", "HOME=~"},
		// The case the naive prefix replace gets wrong: a DIFFERENT account
		// whose name starts with this one. "~bel/x" would be a path that does
		// not exist, reported for a user who is not the one who ran this.
		{"/home/annabel/x", "/home/annabel/x"},
		// Paths outside the home are not user information and must survive
		// whole — they are half of what makes a mount problem diagnosable.
		{"/mnt/nas/peliculas/a.mkv", "/mnt/nas/peliculas/a.mkv"},
		{"/usr/bin/ffmpeg", "/usr/bin/ffmpeg"},
	}
	for _, c := range cases {
		if got := string(s.Text([]byte(c.in))); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A home directory of "/" or "C:" is a broken reading, and substituting it
// would turn every absolute path in the bundle into "~".
func TestHomeRewriteRefusesAShortHome(t *testing.T) {
	for _, home := range []string{"", "/", "C:", "/ho"} {
		s := NewScrubber(config.Default())
		s.rewrites = nil
		if len(home) >= minHomeLen {
			t.Fatalf("test setup: %q is not short", home)
		}
		const line = "/home/anna/Media"
		if got := string(s.Text([]byte(line))); got != line {
			t.Errorf("home %q rewrote %q to %q", home, line, got)
		}
	}
}

func TestScrubErasesEmailAddresses(t *testing.T) {
	s := NewScrubber(config.Default())
	cases := []struct{ in, want string }{
		{"registered as anna.lopez+unarr@example.co.uk [pro]", "registered as " + redactedMarker + " [pro]"},
		{"API error 403: no account for bob@gmail.com", "API error 403: no account for " + redactedMarker},
	}
	for _, c := range cases {
		if got := string(s.Text([]byte(c.in))); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Not everything with an @ is an address, and a scrubber that ate Go module
	// paths would erase the version information out of every stack trace.
	const modPath = "github.com/Unarr-app/unarr-cli@v1.9.0"
	if got := string(s.Text([]byte(modPath))); got != modPath {
		t.Errorf("a module path was mistaken for an email: %q", got)
	}
}

// The order in Text matters: a secret that lives inside a home path has to be
// erased while the path still looks like itself.
func TestScrubHandlesASecretInsideAHomePath(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.APIKey = "sk_live_abcdefghijklmnop"
	s := NewScrubber(cfg)
	s.rewrites = []rewrite{{re: homePattern("/home/anna"), to: homeMarker}}

	got := string(s.Text([]byte("/home/anna/keys/sk_live_abcdefghijklmnop.txt")))
	if strings.Contains(got, "sk_live_abcdefghijklmnop") {
		t.Errorf("the API key survived: %q", got)
	}
	if !strings.HasPrefix(got, "~/keys/") {
		t.Errorf("the home path was not rewritten: %q", got)
	}
}

// Windows writes the same directory both ways, and NTFS does not care about
// case. Asserted through homePattern directly because the variant list is
// built from runtime.GOOS, which a test cannot change.
func TestHomePatternIsAnchoredAtAWordBoundary(t *testing.T) {
	re := homePattern(`C:\Users\Anna`)
	if !re.MatchString(`C:\Users\Anna\AppData`) {
		t.Error("did not match the exact path")
	}
	if re.MatchString(`C:\Users\Annabel\AppData`) {
		t.Error("matched a different account whose name starts the same")
	}
	// A home ending in a separator has an unambiguous end, so no \b is added.
	if got := homePattern(`C:\Users\Anna\`).String(); strings.HasSuffix(got, `\b`) {
		t.Errorf("a separator-terminated home should not need a word boundary: %q", got)
	}
}

// An ABSENT section carries a reason, and that reason is an os.Open error —
// which means it is a full filesystem path. It reaches manifest.json, the file
// a reader opens first, and it used to bypass the scrubber entirely: on the
// machine of someone filing a bug (no daemon yet, no cached benchmark) every
// absent section published the home directory while the section bodies beside
// it were being cleaned.
func TestAbsentSectionReasonIsScrubbed(t *testing.T) {
	s := scrubberWithHome(t, "/home/anna")
	c := collector{"unarr.log", func() ([]byte, error) {
		return nil, errors.New("/home/anna/.local/share/unarr/unarr.log does not exist")
	}}

	got := buildSection(c, s)
	if got.Absent == "" {
		t.Fatal("the section should be recorded as absent")
	}
	if strings.Contains(got.Absent, "/home/anna") {
		t.Errorf("the absent reason leaked the home directory: %q", got.Absent)
	}
	// Still has to say what is missing, or the scrub has destroyed the
	// diagnostic instead of cleaning it.
	if !strings.Contains(got.Absent, "unarr.log") {
		t.Errorf("the absent reason no longer names the file: %q", got.Absent)
	}
}

// The scrubber's own guard: homePattern feeds user-controlled text (a home
// directory can contain regex metacharacters) into a regexp.
func TestHomePatternQuotesMetacharacters(t *testing.T) {
	re := homePattern(`/home/a.b(c)`)
	if !re.MatchString(`/home/a.b(c)/Media`) {
		t.Error("did not match a home containing regex metacharacters")
	}
	if re.MatchString(`/home/axbxcx/Media`) {
		t.Error("the metacharacters were left live — '.' matched any byte")
	}
	if _, err := regexp.Compile(re.String()); err != nil {
		t.Errorf("produced an uncompilable pattern: %v", err)
	}
}
