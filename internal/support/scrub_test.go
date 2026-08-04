package support

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

func TestScrubberErasesPatternsItWasNotToldAbout(t *testing.T) {
	s := NewScrubber(config.Default())

	cases := []struct {
		name string
		in   string
		gone string
		kept string
	}{
		// A stream token is minted at runtime and never passes through Config,
		// so only the pattern pass can catch it.
		{"stream token", "GET /hls/x.m3u8?t=abcdef1234567890 200", "abcdef1234567890", "/hls/x.m3u8?t="},
		{"bearer", "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9", "eyJhbGciOiJIUzI1NiJ9", "Bearer "},
		{"key=value", `api_key=sk_live_0123456789abcdef`, "sk_live_0123456789abcdef", "api_key="},
		{"wireguard key", "peer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= up", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "peer "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(s.Text([]byte(tc.in)))
			if strings.Contains(got, tc.gone) {
				t.Errorf("secret survived: %s", got)
			}
			if !strings.Contains(got, tc.kept) {
				t.Errorf("scrubber ate the context, leaving nothing to read: %s", got)
			}
		})
	}
}

// TestScrubberKeepsOrdinarySettings is the false-positive guard. The bundle is
// evidence; a scrubber that rewrites `require_stream_token = true` has
// destroyed the very line someone is trying to read.
func TestScrubberKeepsOrdinarySettings(t *testing.T) {
	s := NewScrubber(config.Default())
	in := "require_stream_token = true\nlog_level = \"info\"\nmax_concurrent = 3\n/mnt/media/Secretariat.1973.mkv\n"
	if got := string(s.Text([]byte(in))); got != in {
		t.Errorf("scrubber altered ordinary content:\n got: %q\nwant: %q", got, in)
	}
}

// TestScrubberErasesTheDoctorKeyPreview covers the one place a partial
// credential is printed by design: `unarr doctor` shows the first 8 characters
// of the API key, and that string lands in doctor.json verbatim.
func TestScrubberErasesTheDoctorKeyPreview(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.APIKey = "sk_live_verysecretvalue"
	s := NewScrubber(cfg)

	got := string(s.Text([]byte("API key configured: " + cfg.Auth.APIKey[:8] + "...")))
	if strings.Contains(got, "sk_live_") {
		t.Errorf("the doctor key preview survived: %s", got)
	}
}

// TestScrubbedJSONStillParses: the marker carries no quote, brace or
// backslash, so scrubbing doctor.json cannot turn it into something `jq`
// refuses to open.
func TestScrubbedJSONStillParses(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.APIKey = "sk_live_verysecretvalue"
	s := NewScrubber(cfg)

	in := `{"message":"key sk_live_verysecretvalue rejected","url":"https://x/y?t=abcdef1234567890"}`
	var out map[string]string
	if err := json.Unmarshal(s.Text([]byte(in)), &out); err != nil {
		t.Fatalf("scrubbed JSON no longer parses: %v", err)
	}
	if strings.Contains(out["message"], "sk_live") {
		t.Errorf("secret survived in JSON: %v", out)
	}
}

// TestShortSecretsAreNotScrubbedFromText documents a deliberate limit: a
// 3-character WebDAV username occurs inside ordinary words, and rewriting
// "bobbin.mkv" would corrupt the evidence. Short credentials are still withheld
// from config.redacted.toml — this bound applies only to the free-text pass.
func TestShortSecretsAreNotScrubbedFromText(t *testing.T) {
	cfg := config.Default()
	cfg.Download.WebDAVUsername = "bob"
	s := NewScrubber(cfg)

	if got := string(s.Text([]byte("/media/bobbin.mkv"))); got != "/media/bobbin.mkv" {
		t.Errorf("short credential scrubbing corrupted a path: %s", got)
	}
	out, err := configTOML(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "bob") {
		t.Errorf("short credential reached the redacted config:\n%s", out)
	}
}

// TestPassesDoNotStackOnEachOther: the literal pass runs first and leaves
// "[REDACTED]" behind; the pattern pass must not then treat that marker as a
// fresh secret and redact it again into "[REDACTED]]".
func TestPassesDoNotStackOnEachOther(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Hash = "HASHVALUE9876543210"
	s := NewScrubber(cfg)

	got := string(s.Text([]byte("registering agent_hash=" + cfg.Agent.Hash)))
	if want := "registering agent_hash=" + redactedMarker; got != want {
		t.Errorf("double-redaction artefact\n got: %q\nwant: %q", got, want)
	}
}
