package cmd

import (
	"encoding/json"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/engine"
)

// No flags means all three; naming any section means only those. Getting this
// backwards would make `unarr bench --disk` on a headless NAS pay for an
// ffmpeg probe it explicitly did not ask for.
func TestBenchSelectionResolve(t *testing.T) {
	cases := []struct {
		name string
		in   benchSelection
		want benchSelection
	}{
		{"no flags runs everything", benchSelection{}, benchSelection{encode: true, disk: true, net: true}},
		{"disk only", benchSelection{disk: true}, benchSelection{disk: true}},
		{"encode and net", benchSelection{encode: true, net: true}, benchSelection{encode: true, net: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.resolve(); got != tc.want {
				t.Errorf("resolve() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A skipped section must be ABSENT from the JSON, not present-and-zero: a
// consumer cannot tell "0 MB/s" from "never ran" once the key exists.
func TestBenchReportJSONOmitsSkippedSections(t *testing.T) {
	rep := benchReport{
		Disk:  &engine.DiskBenchResult{Dir: "/tmp", MBPerSec: 412},
		Notes: []string{"net: needs a running daemon"},
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["encode"]; ok {
		t.Error("encode key present for a section that never ran")
	}
	if _, ok := decoded["net"]; ok {
		t.Error("net key present for a section that never ran")
	}
	if _, ok := decoded["disk"]; !ok {
		t.Error("disk section missing")
	}
}

// The verdict is the sentence a user acts on, so each outcome must say
// something different — and an unmeasured default must never read as a
// measurement.
func TestEncodeVerdictDistinguishesOutcomes(t *testing.T) {
	seen := map[string]string{}
	for _, res := range []engine.EncodeBenchmark{
		{Reason: engine.EncodeReasonHardware, HWAccel: "nvenc", Ceiling: 2160},
		{Reason: engine.EncodeReasonSustained, Ceiling: 720},
		{Reason: engine.EncodeReasonFloor, Ceiling: 480},
		{Reason: engine.EncodeReasonUnmeasurable, Ceiling: 1080},
		{Reason: engine.EncodeReasonNoFFmpeg, Ceiling: 1080},
	} {
		v := encodeVerdict(res)
		if v == "" {
			t.Fatalf("reason %q produced an empty verdict", res.Reason)
		}
		if prev, dup := seen[v]; dup {
			t.Errorf("reason %q reuses the verdict of %q", res.Reason, prev)
		}
		seen[v] = res.Reason
	}
}

func TestEncodeBackendLabel(t *testing.T) {
	if got := encodeBackendLabel(engine.EncodeBenchmark{HWAccel: string(engine.HWAccelNone)}); got != "software libx264" {
		t.Errorf("none = %q, want software libx264", got)
	}
	if got := encodeBackendLabel(engine.EncodeBenchmark{}); got != "software libx264" {
		t.Errorf("empty = %q, want software libx264", got)
	}
	if got := encodeBackendLabel(engine.EncodeBenchmark{HWAccel: "nvenc"}); got != "hwaccel nvenc" {
		t.Errorf("nvenc = %q, want hwaccel nvenc", got)
	}
}
