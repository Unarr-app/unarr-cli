package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/testutil"
)

// A trimmed but structurally faithful `ffmpeg -encoders` table: the legend
// block above the separator has the same 6-char first column as a real row, and
// libfdk_aac / aac_at are the exact names that make a naive strings.Contains
// check report "aac present" on a build that has no plain aac encoder.
const fakeEncoderTable = `Encoders:
 V..... = Video
 A..... = Audio
 ------
 V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC
 A....D aac                  AAC (Advanced Audio Coding)
 A....D libfdk_aac           Fraunhofer FDK AAC
 V....D h264_nvenc           NVIDIA NVENC H.264 encoder
`

func TestFFmpegEncoderNamesParsesNameColumn(t *testing.T) {
	names := ffmpegEncoderNames(fakeEncoderTable)

	for _, want := range []string{"libx264", "aac", "libfdk_aac", "h264_nvenc"} {
		if !names[want] {
			t.Errorf("encoder %q not parsed out of the table", want)
		}
	}
	// The legend rows must not become encoders.
	if names["="] || names["Video"] || names["Encoders:"] {
		t.Errorf("legend leaked into the encoder set: %v", names)
	}
}

func TestMissingEncoders(t *testing.T) {
	tests := []struct {
		name  string
		table string
		want  []string
	}{
		{"complete build", fakeEncoderTable, nil},
		{
			// The regression this parser exists for: only the fdk variant.
			name:  "aac only as libfdk_aac",
			table: " V....D libx264   x264\n A....D libfdk_aac   FDK\n",
			want:  []string{"aac"},
		},
		{"audio-only build", " A....D aac   AAC\n", []string{"libx264"}},
		{"empty output", "", []string{"libx264", "aac"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := missingEncoders(ffmpegEncoderNames(tc.table))
			if !slices.Equal(got, tc.want) {
				t.Errorf("missingEncoders = %v, want %v", got, tc.want)
			}
		})
	}
}

// stubTool writes an executable shell stub that answers every ffmpeg probe with
// canned text, so the probe can be exercised on a host whose real ffmpeg build
// we do not control.
func stubTool(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProbeMediaNoFFmpegIsAnEmptyProbeNotAnError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p := ProbeMedia(ctx, "", "")

	if p.FFmpegPath != "" || p.FFmpegVersion != "" {
		t.Errorf("expected an empty probe, got %+v", p)
	}
	if p.EncodersProbed {
		t.Error("nothing should have been probed without a binary")
	}
	if p.Zscale || p.HW.Pick != HWAccelNone {
		t.Errorf("absent ffmpeg must not claim capabilities: %+v", p)
	}
}

func TestProbeMediaReportsWhatTheBinaryAnswers(t *testing.T) {
	testutil.RequireShellStubs(t)
	ResetHWAccelCache()
	t.Cleanup(ResetHWAccelCache)

	// One stub, dispatching on the ffmpeg subcommand the probe passes.
	ffmpeg := stubTool(t, "ffmpeg", `
case "$2" in
  -version)  echo "ffmpeg version 9.9.9-stub Copyright (c) 2000-2026 the FFmpeg developers" ;;
  -encoders) echo " V....D libx264   x264"; echo " A....D libfdk_aac   FDK" ;;
  -filters)  echo " ... scale            V->V   scale the input" ;;
esac
`)
	ffprobe := stubTool(t, "ffprobe", `echo "ffprobe version 9.9.9-stub Copyright (c) 2000-2026"`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := ProbeMedia(ctx, ffmpeg, ffprobe)

	if p.FFmpegVersion != "ffmpeg version 9.9.9-stub" {
		t.Errorf("FFmpegVersion = %q", p.FFmpegVersion)
	}
	if p.FFprobeVersion != "ffprobe version 9.9.9-stub" {
		t.Errorf("FFprobeVersion = %q", p.FFprobeVersion)
	}
	if !p.EncodersProbed {
		t.Fatal("encoders were listed, EncodersProbed must be true")
	}
	if !slices.Equal(p.MissingEncoders, []string{"aac"}) {
		t.Errorf("MissingEncoders = %v, want [aac] (libfdk_aac is not aac)", p.MissingEncoders)
	}
	if p.Zscale {
		t.Error("zscale must be false when -filters does not list it")
	}
	// h264_nvenc is absent from the stub's table, so no device check can
	// promote this host off software.
	if p.HW.Pick != HWAccelNone {
		t.Errorf("HW.Pick = %q, want none", p.HW.Pick)
	}
}

func TestProbeMediaBrokenBinaryDegradesToUnknown(t *testing.T) {
	testutil.RequireShellStubs(t)
	ResetHWAccelCache()
	t.Cleanup(ResetHWAccelCache)

	// Present on disk, exits non-zero for everything — the "wrong architecture
	// / missing shared library" shape. The probe must report emptiness, not
	// invent capabilities and not hang.
	broken := stubTool(t, "ffmpeg", "exit 1\n")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := ProbeMedia(ctx, broken, "")

	if p.FFmpegPath != broken {
		t.Errorf("FFmpegPath = %q, want %q", p.FFmpegPath, broken)
	}
	if p.FFmpegVersion != "" {
		t.Errorf("a binary that exits 1 must report no version, got %q", p.FFmpegVersion)
	}
	if p.EncodersProbed {
		t.Error("EncodersProbed must stay false when -encoders fails")
	}
	if p.Zscale {
		t.Error("Zscale must be false when -filters fails")
	}
}

func TestFFmpegHasFilterStub(t *testing.T) {
	testutil.RequireShellStubs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	with := stubTool(t, "ffmpeg", "echo ' .SC zscale           V->V'\n")
	if !ffmpegHasFilter(ctx, with, "zscale") {
		t.Error("expected true when -filters lists zscale")
	}
	without := stubTool(t, "ffmpeg", "echo ' ... scale            V->V'\n")
	if ffmpegHasFilter(ctx, without, "zscale") {
		t.Error("expected false when -filters omits zscale")
	}
	if ffmpegHasFilter(ctx, "", "zscale") {
		t.Error("empty path must be false")
	}
	if ffmpegHasFilter(ctx, "/nonexistent/ffmpeg", "zscale") {
		t.Error("nonexistent binary must be false")
	}
}
