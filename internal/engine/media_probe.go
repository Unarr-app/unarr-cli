package engine

import (
	"bytes"
	"context"
	"os/exec"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// RequiredEncoders are the encoders the HLS pipeline names directly in every
// transcode command it builds (see buildFFmpegArgs / buildHLSFFmpegArgsAt). An
// ffmpeg build without them is not "slower", it is unable to produce a segment
// the browser can play — which is why doctor treats their absence as a failure
// rather than a warning.
var RequiredEncoders = []string{"libx264", "aac"}

// MediaProbe is one snapshot of the host's ffmpeg toolchain: everything the
// streaming path silently depends on, gathered in a single pass so a caller
// asking six questions pays for one round of subprocesses instead of six.
//
// Every field degrades to its zero value rather than to an error: a probe of a
// host with no ffmpeg is a valid, meaningful result ("nothing here"), and the
// caller decides which of those zeros is a failure and which is a warning.
type MediaProbe struct {
	FFmpegPath     string
	FFmpegVersion  string // "" when the binary is missing or would not run
	FFprobePath    string
	FFprobeVersion string

	// EncodersProbed distinguishes "we asked ffmpeg and libx264 is genuinely
	// absent" from "ffmpeg -encoders never ran", which are different problems
	// with different fixes and must not print the same message.
	EncodersProbed  bool
	MissingEncoders []string

	Zscale bool // libzimg built in — no zscale means no HDR→SDR tonemap
	HW     HWAccelDiagnostic
}

// ProbeMedia gathers the toolchain snapshot. Paths come from the caller (which
// owns config + the mediainfo locator) so this stays a pure capability probe
// with no opinion about where a binary should have been found.
//
// Cost is bounded by ctx and by what each probe is: `-version`, `-encoders` and
// `-filters` only list what the binary was compiled with. Nothing here encodes
// a frame — `unarr doctor` is interactive, and a diagnostic that benchmarks is
// a diagnostic nobody runs.
func ProbeMedia(ctx context.Context, ffmpegPath, ffprobePath string) MediaProbe {
	// Pick is seeded to "none" rather than left as the empty string so an
	// early return still describes a host honestly ("software only"), and no
	// consumer has to know that "" and "none" mean the same thing.
	p := MediaProbe{
		FFmpegPath:  ffmpegPath,
		FFprobePath: ffprobePath,
		HW:          HWAccelDiagnostic{Pick: HWAccelNone},
	}
	if ffprobePath != "" {
		p.FFprobeVersion = toolVersionLine(ctx, ffprobePath)
	}
	if ffmpegPath == "" {
		return p
	}

	// DetectHWAccelDiagnostic already runs `-version` and `-encoders`; reusing
	// it keeps doctor and the daemon startup log reporting the same verdict
	// from the same code, which is the point of the diagnostic type existing.
	p.HW = DetectHWAccelDiagnostic(ctx, ffmpegPath)
	p.FFmpegVersion = p.HW.FFmpegVersion

	if encoders := listFFmpegEncoders(ctx, ffmpegPath); encoders != "" {
		p.EncodersProbed = true
		p.MissingEncoders = missingEncoders(ffmpegEncoderNames(encoders))
	}
	p.Zscale = ffmpegHasFilter(ctx, ffmpegPath, "zscale")
	return p
}

// ffmpegEncoderNames extracts the encoder NAMES from `ffmpeg -encoders` output.
//
// Parsing the name column rather than substring-matching the whole blob is not
// pedantry: "aac" is a substring of "libfdk_aac" and "aac_at", so a Contains
// check would call a build AAC-capable when the only encoder it ships is one
// ffmpeg will never select under the name our transcode args pass.
//
// Layout is "<6 flag chars> <name> <description>", e.g. " A....D aac  AAC…".
// The legend block above the table ("V..... = Video") also has a 6-char first
// field, hence the "=" guard.
func ffmpegEncoderNames(out string) map[string]bool {
	names := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields[0]) != 6 || fields[1] == "=" {
			continue
		}
		names[fields[1]] = true
	}
	return names
}

// missingEncoders returns the RequiredEncoders absent from the parsed set, in
// declaration order so the message reads the same on every host.
func missingEncoders(have map[string]bool) []string {
	var missing []string
	for _, want := range RequiredEncoders {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	return missing
}

// ffmpegHasFilter reports whether the binary lists the named filter in
// `ffmpeg -filters`. Silent, uncached and ctx-bounded — it is the shared probe
// behind FFmpegSupportsZscale (which wraps it in the per-path cache and the
// startup log the streaming path wants) and behind ProbeMedia (which wants a
// fresh answer and no log line landing in the middle of the doctor report).
func ffmpegHasFilter(ctx context.Context, ffmpegPath, filter string) bool {
	if ffmpegPath == "" {
		return false
	}
	cmd := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-filters")
	winproc.HideWindow(cmd)
	out, err := cmd.Output()
	return err == nil && bytes.Contains(out, []byte(filter))
}
