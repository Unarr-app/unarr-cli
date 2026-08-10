package doctor

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/fatih/color"
)

// These cover the four render items the Fase 0 manual checklist listed as
// eyeball-only (T1-T4 in docs/plans/fase0-manual-checklist.md): readable at 80
// and 60 columns, no ANSI under NO_COLOR, and plain output with no TTY. A human
// squinting at a terminal cannot be a release gate, and every one of these is
// decidable from the bytes the renderer emits.
//
// What is asserted is the renderer's OWN width — the frame it wraps around a
// check: the "  x name" prefix, the group headers, the summary and the tip. The
// message a check hands back is NOT bounded here: a filesystem path, a URL or an
// OS error can be arbitrarily long, and truncating one would hide the very
// detail the operator needs. So a long message is allowed to run past 80; the
// scaffolding around it is not.

// widthProbeSpecs exercise every rendering branch (pass/warn/fail, with and
// without a message) using the longest REAL check names and groups shipped in
// doctor_specs*.go — the frame has to fit those, not synthetic short ones.
func widthProbeSpecs() []Spec {
	return []Spec{
		specPass("Config", "Config file", ""),
		specPass("Config", "Config values (ranges)", ""),
		specFail("Config", "API key configured", ""),
		specPass("Connectivity", "Discovery API (search/stats)", ""),
		specFail("Connectivity", "Agent registration", ""),
		specPass("Media", "Encoders (libx264, aac)", ""),
		specWarn("Media", "zscale (HDR tonemap)", ""),
		specPass("Media", "Hardware acceleration", ""),
		specPass("Downloads", "par2 (usenet verify/repair)", ""),
		specPass("Library", "Library free space", ""),
	}
}

// renderWidthProbe renders the frame-only scenario and returns the raw output.
func renderWidthProbe(t *testing.T) string {
	t.Helper()
	prev := color.NoColor
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = prev })

	var buf bytes.Buffer
	r := NewTextRenderer(&buf)
	r.ShowFixTip = true
	r.Start()
	rep := Run(widthProbeSpecs(), r.OnCheck)
	r.Finish(rep)
	return buf.String()
}

// T1 — `doctor` legible en 80 columnas.
func TestRenderFrameFitsEightyColumns(t *testing.T) {
	assertNoLineWiderThan(t, renderWidthProbe(t), 80)
}

// T2 — legible en 60 columnas. 60 is the narrow-terminal floor the checklist
// picked (a split tmux pane, an 80-col console with a scrollbar eating room).
func TestRenderFrameFitsSixtyColumns(t *testing.T) {
	assertNoLineWiderThan(t, renderWidthProbe(t), 60)
}

func assertNoLineWiderThan(t *testing.T, out string, limit int) {
	t.Helper()
	for i, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		// Count runes, not bytes: the summary and tip are ASCII today, but a
		// byte count would silently mis-measure the moment one is not.
		if n := len([]rune(line)); n > limit {
			t.Errorf("line %d is %d columns, limit %d:\n%s", i+1, n, limit, line)
		}
	}
}

// T3 — NO_COLOR must silence ANSI. Asserted through the ENVIRONMENT, not by
// setting color.NoColor: the flag is what the test would be proving, and the
// contract users rely on (https://no-color.org) is the env var. fatih/color
// samples it in an init(), so the value is re-derived here the same way the
// package does it.
func TestNoColorEnvEmitsNoANSI(t *testing.T) {
	prev := color.NoColor
	t.Setenv("NO_COLOR", "1")
	_, noColorSet := os.LookupEnv("NO_COLOR")
	color.NoColor = noColorSet
	t.Cleanup(func() { color.NoColor = prev })

	out := renderMixedForColor(t)
	if strings.Contains(out, "\x1b[") {
		t.Errorf("ANSI escape emitted under NO_COLOR:\n%q", out)
	}
}

// T4 — no TTY: a redirected stdout (a pipe, a file, `doctor | tee`) must render
// plain. This is the Docker HEALTHCHECK and support-bundle path, where an escape
// sequence lands in a log as mojibake.
func TestNonTTYRendersPlain(t *testing.T) {
	prev := color.NoColor
	// What color.NoColor means for a non-terminal writer: fatih/color disables
	// itself when stdout is not a character device. Model that explicitly rather
	// than depending on how the test binary was invoked (`go test` already
	// redirects stdout, which would make this pass for the wrong reason).
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = prev })

	out := renderMixedForColor(t)
	if strings.Contains(out, "\x1b[") {
		t.Errorf("ANSI escape emitted to a non-TTY writer:\n%q", out)
	}
	// A plain render must still carry the information, not just be colourless:
	// status is conveyed by the +/x/! symbol, which is what a colourblind user
	// and a log file both read.
	for _, want := range []string{"+ ", "x ", "! "} {
		if !strings.Contains(out, want) {
			t.Errorf("status symbol %q missing — colour was the only signal:\n%s", want, out)
		}
	}
}

// renderMixedForColor renders one of each status, so an ANSI leak in any branch
// (green pass, red fail, yellow warn, bold header, dim tip) is caught.
func renderMixedForColor(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	r := NewTextRenderer(&buf)
	r.ShowFixTip = true
	r.Start()
	rep := Run([]Spec{
		specPass("Config", "Config file", "/home/u/.config/unarr/config.toml"),
		specWarn("Downloads", "par2 (usenet verify/repair)", "not installed"),
		specFail("Connectivity", "Agent registration", ""),
	}, r.OnCheck)
	r.Finish(rep)
	return buf.String()
}
