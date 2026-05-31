package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hlsArgsFor(hdr string, tonemap bool, hw HWAccel) string {
	cfg := HLSSessionConfig{
		SessionID:  "test",
		SourcePath: "/movies/x.mkv",
		Quality:    "720p",
		Transcode: TranscodeRuntime{
			FFmpegPath:  "/usr/bin/ffmpeg",
			FFprobePath: "/usr/bin/ffprobe",
			HWAccel:     hw,
			TonemapHDR:  tonemap,
		},
	}
	probe := &StreamProbe{Width: 3840, Height: 2160, BitDepth: 10, HDR: hdr, DurationSec: 100}
	return strings.Join(buildHLSFFmpegArgsAt(cfg, probe, "/tmp/t", 0, 0), " ")
}

func vfChain(joined string) string {
	parts := strings.Split(joined, " ")
	for i, p := range parts {
		if p == "-vf" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func TestTonemap_AppliedForHDRWhenSupported(t *testing.T) {
	vf := vfChain(hlsArgsFor("HDR10", true, HWAccelNone))
	if !strings.Contains(vf, "zscale=t=linear") || !strings.Contains(vf, "tonemap=tonemap=hable") {
		t.Fatalf("HDR + zscale-capable: expected tonemap in -vf, got %q", vf)
	}
	// Order: a scale filter, then tonemap (zscale), then format=.
	scaleIdx := strings.Index(vf, "scale=")
	zIdx := strings.Index(vf, "zscale=t=linear")
	fmtIdx := strings.Index(vf, "format=")
	if !(scaleIdx >= 0 && scaleIdx < zIdx && zIdx < fmtIdx) {
		t.Errorf("filter order wrong (scale < tonemap < format): %q", vf)
	}
}

func TestTonemap_AppliedInNoDownscaleBranch(t *testing.T) {
	// Source already within the quality cap → no downscale; tonemap must still
	// be inserted before format=.
	cfg := HLSSessionConfig{
		SessionID:  "test",
		SourcePath: "/movies/x.mkv",
		Quality:    "2160p",
		Transcode:  TranscodeRuntime{FFmpegPath: "/usr/bin/ffmpeg", HWAccel: HWAccelNone, TonemapHDR: true},
	}
	probe := &StreamProbe{Width: 3840, Height: 2160, HDR: "HDR10", DurationSec: 100}
	vf := vfChain(strings.Join(buildHLSFFmpegArgsAt(cfg, probe, "/tmp/t", 0, 0), " "))
	if !strings.Contains(vf, "tonemap=tonemap=hable") {
		t.Errorf("no-downscale branch: expected tonemap, got %q", vf)
	}
	if z, f := strings.Index(vf, "zscale=t=linear"), strings.Index(vf, "format="); !(z >= 0 && z < f) {
		t.Errorf("tonemap must precede format=: %q", vf)
	}
}

func TestTonemap_SkippedWhenFFmpegLacksZscale(t *testing.T) {
	vf := vfChain(hlsArgsFor("HDR10", false, HWAccelNone))
	if strings.Contains(vf, "zscale") || strings.Contains(vf, "tonemap") {
		t.Errorf("ffmpeg without zscale: tonemap must be skipped, got %q", vf)
	}
}

func TestTonemap_SkippedForSDR(t *testing.T) {
	vf := vfChain(hlsArgsFor("", true, HWAccelNone))
	if strings.Contains(vf, "zscale") || strings.Contains(vf, "tonemap") {
		t.Errorf("SDR source: no tonemap expected, got %q", vf)
	}
}

func TestTonemap_VAAPIInsertsBeforeHwupload(t *testing.T) {
	vf := vfChain(hlsArgsFor("HDR10", true, HWAccelVAAPI))
	if !strings.Contains(vf, "tonemap=tonemap=hable") {
		t.Fatalf("VAAPI HDR: expected tonemap, got %q", vf)
	}
	// Tonemap is a CPU filter — must run before the GPU upload.
	if up := strings.Index(vf, "hwupload"); up >= 0 {
		if strings.Index(vf, "zscale=t=linear") > up {
			t.Errorf("tonemap must precede hwupload: %q", vf)
		}
	}
}

func TestFFmpegSupportsZscale_Stub(t *testing.T) {
	dir := t.TempDir()

	withZ := filepath.Join(dir, "ffmpeg-with.sh")
	if err := os.WriteFile(withZ, []byte("#!/bin/sh\necho ' .SC zscale           V->V'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !FFmpegSupportsZscale(withZ) {
		t.Error("expected true for an ffmpeg whose -filters lists zscale")
	}

	noZ := filepath.Join(dir, "ffmpeg-without.sh")
	if err := os.WriteFile(noZ, []byte("#!/bin/sh\necho ' ... scale            V->V'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if FFmpegSupportsZscale(noZ) {
		t.Error("expected false for an ffmpeg whose -filters omits zscale")
	}

	if FFmpegSupportsZscale("") {
		t.Error("empty path must be false")
	}
}
