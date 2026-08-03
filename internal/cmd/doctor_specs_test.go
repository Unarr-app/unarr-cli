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
		{"Config", "API key configured"},
		{"Connectivity", "API reachable"},
		{"Connectivity", "Discovery API (search/stats)"},
		{"Connectivity", "Agent registration"},
		{"Downloads", "Download directory"},
		{"Downloads", "Download dir writable"},
		{"Downloads", "Disk space"},
		{"Downloads", "par2 (usenet verify/repair)"},
		{"Downloads", "Managed VPN (P2P kill-switch)"},
		{"Media", "ffmpeg"},
		{"Media", "ffprobe"},
		{"Media", "Encoders (libx264, aac)"},
		{"Media", "zscale (HDR tonemap)"},
		{"Media", "Hardware acceleration"},
		{"Media", "Transcode ceiling"},
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
