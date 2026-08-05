package cmd

import (
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/doctor"
)

// The spec list is the doctor report's contract: order is display order and the
// group is the console section header. Building the list is pure (no check runs
// here), so this guards against a reorder or a dropped check.
func TestDoctorSpecsOrderAndGroups(t *testing.T) {
	withDataDir(t)

	cfg := config.Default()
	specs := doctorSpecs(&cfg)

	want := [][2]string{
		{"Config", "Config file"},
		{"Config", "Config keys"},
		{"Config", "Config values"},
		{"Config", "API key configured"},
		{"Connectivity", "API reachable"},
		{"Connectivity", "Discovery API (search/stats)"},
		{"Connectivity", "Agent registration"},
		{"Downloads", "Download directory"},
		{"Downloads", "Download dir writable"},
		{"Downloads", "Disk space"},
		{"Downloads", "par2 (usenet verify/repair)"},
		{"Downloads", "Managed VPN (P2P kill-switch)"},
		{"Library", "Library directories"},
		{"Library", "Library free space"},
		{"Streaming", "Stream port"},
		{"Streaming", "HTTPS stream port"},
		{"Streaming", "Reachable from the LAN"},
		{"Media", "ffmpeg"},
		{"Media", "ffprobe"},
		{"Media", "Encoders (libx264, aac)"},
		{"Media", "zscale (HDR tonemap)"},
		{"Media", "Hardware acceleration"},
		{"Media", "Transcode ceiling"},
		{"Daemon", "Daemon process"},
		{"Version", "unarr version"},
	}
	if len(specs) != len(want) {
		t.Fatalf("got %d specs, want %d", len(specs), len(want))
	}
	for i, w := range want {
		if specs[i].Group != w[0] || specs[i].Name != w[1] {
			t.Errorf("spec %d = %q/%q, want %q/%q", i, specs[i].Group, specs[i].Name, w[0], w[1])
		}
		if specs[i].Fn == nil {
			t.Errorf("spec %d (%s) has no Fn", i, specs[i].Name)
		}
	}
}

// The --quick subset is what a Docker HEALTHCHECK runs every 60 seconds, and
// the one thing it must never do is touch the network: a probe that calls the
// API turns an ISP blip into a container restart loop.
//
// This pins the membership by name. Spec.Quick defaults to false, so a check
// added later is excluded until someone states otherwise — the failure mode of
// forgetting the field is a probe that tests too little, never one that
// restarts a healthy container. Adding a name here is therefore a deliberate
// act, and this test is where it gets reviewed.
func TestQuickSpecsAreLocalOnly(t *testing.T) {
	withDataDir(t)

	cfg := config.Default()
	quick := doctor.QuickSpecs(doctorSpecs(&cfg))

	want := []string{
		"Config file",
		"Config keys",
		"Config values",
		"Download directory",
		"Download dir writable",
		"Disk space",
		"Daemon process",
		"unarr version",
	}
	got := make([]string, len(quick))
	for i, s := range quick {
		got[i] = s.Name
	}
	if len(got) != len(want) {
		t.Fatalf("quick specs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("quick spec %d = %q, want %q", i, got[i], want[i])
		}
	}

	// Belt and braces: every check that talks to the API must be absent, named
	// explicitly so a rename cannot silently let one slip into the probe.
	for _, s := range quick {
		switch s.Name {
		case "API reachable", "Discovery API (search/stats)", "Agent registration":
			t.Errorf("%q makes a network call and must not be in the --quick set", s.Name)
		}
	}
}

// Only offline specs are exercised here — the connectivity ones would hit the
// network. They still have to honour the (message, error) convention.
func TestDoctorVersionSpecPasses(t *testing.T) {
	withDataDir(t)

	cfg := config.Default()
	specs := doctorSpecs(&cfg)
	last := specs[len(specs)-1]

	rep := doctor.Run([]doctor.Spec{last}, nil)
	if rep.Status != doctor.StatusPass || rep.Passed != 1 {
		t.Fatalf("version check = %+v, want a single pass", rep)
	}
	if rep.Checks[0].Message == "" {
		t.Error("version check reported no version string")
	}
}
