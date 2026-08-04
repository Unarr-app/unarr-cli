package doctor

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fatih/color"
)

// The golden files were generated from the pre-refactor runDoctor printing code
// (banner + check() closure + summary + tip, verbatim). They are the contract:
// `unarr doctor`'s console output must stay byte-for-byte identical. If a change
// here makes them fail, the output regressed — fix the code, not the golden.

func specPass(group, name, msg string) Spec {
	return Spec{Group: group, Name: name, Fn: func() (string, error) { return msg, nil }}
}

func specWarn(group, name, msg string) Spec {
	return Spec{Group: group, Name: name, Fn: func() (string, error) { return "!" + msg, nil }}
}

func specFail(group, name, msg string) Spec {
	return Spec{Group: group, Name: name, Fn: func() (string, error) { return msg, errors.New("boom") }}
}

// Covers every rendering branch: pass with and without a message, fail with and
// without a message, warn, and four group headers.
var (
	goldConfigFile = specPass("Config", "Config file", "/home/u/.config/unarr/config.toml")
	goldConfigKeys = specPass("Config", "Config keys", "")
	goldAPIReach   = specPass("Connectivity", "API reachable", "https://unarr.app (42ms)")
	goldVersion    = specPass("Version", "unarr version", "1.8.2 (linux/amd64)")
	goldPar2       = specWarn("Downloads", "par2 (usenet verify/repair)", "not installed — usenet downloads are delivered UNVERIFIED")
	goldAPIKey     = specFail("Config", "API key configured", "run `unarr init` to configure it")
	goldAgent      = specFail("Connectivity", "Agent registration", "")
)

func goldenSpecs(scenario string) []Spec {
	switch scenario {
	case "all_pass":
		return []Spec{goldConfigFile, goldConfigKeys, goldAPIReach, goldVersion}
	case "warn":
		return []Spec{goldConfigFile, goldConfigKeys, goldAPIReach, goldPar2, goldVersion}
	default:
		return []Spec{goldConfigFile, goldConfigKeys, goldAPIKey, goldAPIReach, goldAgent, goldPar2, goldVersion}
	}
}

func TestTextRendererGolden(t *testing.T) {
	prev := color.NoColor
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = prev })

	for _, scenario := range []string{"all_pass", "warn", "mixed"} {
		t.Run(scenario, func(t *testing.T) {
			var buf bytes.Buffer
			r := NewTextRenderer(&buf)
			r.ShowFixTip = true
			r.Start()
			rep := Run(goldenSpecs(scenario), r.OnCheck)
			r.Finish(rep)

			path := filepath.Join("testdata", "render_text_"+scenario+".golden")
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if got := buf.String(); got != string(want) {
				t.Errorf("output drifted from %s\n--- got ---\n%q\n--- want ---\n%q", path, got, want)
			}
		})
	}
}

// The fix tip is suppressed during --fix, which is about to repair anyway.
func TestTextRendererFixTipSuppressed(t *testing.T) {
	prev := color.NoColor
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = prev })

	var buf bytes.Buffer
	r := NewTextRenderer(&buf)
	rep := Run([]Spec{goldAPIKey}, r.OnCheck)
	r.Finish(rep)

	if bytes.Contains(buf.Bytes(), []byte("doctor --fix")) {
		t.Errorf("tip printed with ShowFixTip=false:\n%s", buf.String())
	}
}

// Checks must reach the renderer as they complete, not in one batch at the end:
// the connectivity checks take up to 10 s each and doctor would look hung.
func TestRunStreamsChecks(t *testing.T) {
	prev := color.NoColor
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = prev })

	var buf bytes.Buffer
	r := NewTextRenderer(&buf)
	seen := make([]string, 0, 2)

	specs := []Spec{
		{Group: "G", Name: "first", Fn: func() (string, error) { return "ok", nil }},
		{Group: "G", Name: "second", Fn: func() (string, error) {
			// By the time this runs, "first" must already be on the wire.
			if !bytes.Contains(buf.Bytes(), []byte("first")) {
				t.Errorf("second check started before the first was rendered: %q", buf.String())
			}
			return "ok", nil
		}},
	}
	Run(specs, func(c Check) {
		seen = append(seen, c.Name)
		r.OnCheck(c)
	})

	if len(seen) != 2 || seen[0] != "first" || seen[1] != "second" {
		t.Errorf("callback order = %v, want [first second]", seen)
	}
}
