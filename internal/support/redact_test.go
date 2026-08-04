package support

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// TestEveryConfigFieldIsClassified is the mechanical half of the redaction
// guarantee, and it is meant to fail.
//
// Adding a field to config.Config — say `[usenet] password` — makes this test
// red until someone writes down, in configFields, whether it may be published.
// That is the whole mitigation: the protection cannot decay into "we forgot",
// because forgetting is a build failure rather than a silent leak.
//
// It fails in both directions. A missing entry is the dangerous one; a stale
// entry (a field that was renamed or removed) is the one that would otherwise
// leave configFields looking complete while covering nothing.
func TestEveryConfigFieldIsClassified(t *testing.T) {
	actual := leafPaths(reflect.TypeOf(config.Config{}))

	for _, path := range actual {
		if _, ok := configFields[path]; !ok {
			t.Errorf("config.Config field %q is NOT classified in configFields (internal/support/redact.go).\n"+
				"Every field must be declared Publishable or Secret before it can be reasoned about.\n"+
				"If it can hold a credential, a token, a password or anything the user would not paste\n"+
				"into a public GitHub issue, mark it Secret. When unsure, mark it Secret.", path)
		}
	}
	for path := range configFields {
		if !slices.Contains(actual, path) {
			t.Errorf("configFields classifies %q, which no longer exists in config.Config — remove the stale entry", path)
		}
	}
}

// TestSecretFieldsAreTheExpectedOnes pins the credential set. The plan's
// security section names these by hand; if a future refactor reclassifies one
// of them as Publishable, that should take a deliberate edit here too.
func TestSecretFieldsAreTheExpectedOnes(t *testing.T) {
	want := []string{
		"Agent.Hash",
		"Auth.APIKey",
		"Download.WebDAVPassword",
		"Download.WebDAVUsername",
	}
	if got := secretPaths(); !slices.Equal(got, want) {
		t.Errorf("secret fields changed\n got: %v\nwant: %v", got, want)
	}
}

// TestRedactedConfigNeverCopiesFreeText fills every string field of a Config
// with a marker and checks that the rendered TOML contains none of them.
//
// This is the value-level half of the allowlist: it is not enough for the
// bundle to skip the fields we called secret, because a secret pasted into the
// wrong key (a password typed into agent.name, an access token appended to a
// custom api_url) would still travel. Nothing the user typed is copied out.
func TestRedactedConfigNeverCopiesFreeText(t *testing.T) {
	const marker = "USER-TYPED-VALUE-DO-NOT-COPY"
	cfg := config.Default()
	fillStrings(reflect.ValueOf(&cfg).Elem(), marker)

	out, err := configTOML(cfg)
	if err != nil {
		t.Fatalf("render redacted config: %v", err)
	}
	if n := strings.Count(string(out), marker); n != 0 {
		t.Fatalf("redacted config copied a user-typed value %d time(s):\n%s", n, out)
	}
}

// TestRedactedConfigStillSaysSomething is the anti-vacuity guard for the test
// above: a projection that emitted nothing at all would also leak nothing, and
// would also be useless. These are the settings a support answer is built from.
func TestRedactedConfigStillSaysSomething(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.APIKey = "sk_live_abcdefghijklmnop"
	cfg.Download.PreferredMethods = []string{"debrid", "torrent"}
	cfg.Daemon.LogLevel = "debug"
	cfg.Download.Dir = "/mnt/media/downloads"

	out, err := configTOML(cfg)
	if err != nil {
		t.Fatalf("render redacted config: %v", err)
	}
	got := string(out)
	for _, want := range []string{
		`api_key = "<withheld>"`,  // set, and withheld — not merely absent
		`api_url = "<default>"`,   // stock endpoint
		`log_level = "debug"`,     // vocabulary value, published verbatim
		`max_concurrent = 3`,      // numbers carry no user data
		`dir = "<set>"`,           // the path exists, the path is not published
		`"debrid", "torrent"`,     // the method order, which explains most routing bugs
		`min_free_disk_mb = 2048`, //
		`require_stream_token = true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("redacted config is missing %q\n%s", want, got)
		}
	}
}

// TestNonStandardValuesAreFlaggedNotEchoed checks the third state: a value that
// is neither empty nor recognised is reported as non-standard. That is a real
// support finding ("this key is set to something we do not understand") that
// does not require repeating what the user wrote.
func TestNonStandardValuesAreFlaggedNotEchoed(t *testing.T) {
	cfg := config.Default()
	cfg.Daemon.LogLevel = "verbose"
	cfg.Download.StallTimeout = "half an hour"

	out, err := configTOML(cfg)
	if err != nil {
		t.Fatalf("render redacted config: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `log_level = "<non-standard>"`) {
		t.Errorf("unrecognised log_level was not flagged:\n%s", got)
	}
	if strings.Contains(got, "half an hour") {
		t.Errorf("unrecognised duration was echoed verbatim:\n%s", got)
	}
}

// fillStrings writes marker into every string (and []string) leaf reachable
// from v. It walks with the SAME recursion the classification test uses, so
// the two cannot disagree about what a field is — a filler with its own idea
// of "leaf" would silently stop covering the fields it is meant to prove.
func fillStrings(v reflect.Value, marker string) {
	walkLeaves(v.Type(), "", func(path string, _ reflect.StructField) {
		fv := valueAt(v, path)
		if !fv.IsValid() || !fv.CanSet() {
			return
		}
		switch {
		case fv.Kind() == reflect.String:
			fv.SetString(marker)
		case fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() == reflect.String:
			fv.Set(reflect.ValueOf([]string{marker}))
		}
	})
}
