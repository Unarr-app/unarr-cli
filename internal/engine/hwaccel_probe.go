package engine

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// hwProbeTimeout bounds one candidate's test encode. HW device init is the
// slow part (~0.5 s QSV, ~1 s NVENC on a cold driver); 10 s leaves headroom on
// a busy NAS without stalling daemon startup on a wedged driver.
const hwProbeTimeout = 10 * time.Second

// hwProbeArgs is the argv of a one-second 256×256 test encode through hw's
// h264 encoder to the null muxer. VA-API cannot take system-memory frames
// directly, so its frames are uploaded to the device first.
func hwProbeArgs(hw HWAccel) []string {
	args := []string{"-hide_banner", "-nostats", "-loglevel", "error"}
	if hw == HWAccelVAAPI {
		args = append(args, "-vaapi_device", "/dev/dri/renderD128")
	}
	args = append(args, "-f", "lavfi", "-i", "testsrc2=size=256x256:rate=24:duration=1")
	if hw == HWAccelVAAPI {
		args = append(args, "-vf", "format=nv12,hwupload")
	}
	return append(args, "-c:v", hw.FFmpegVideoCodec("h264"), "-f", "null", "-")
}

// probeHWEncoder runs the test encode and returns nil when hw can actually
// open and encode. A probe that runs out of time proves nothing about the
// encoder (slow box, busy driver), so it is NOT treated as a failure — that
// keeps the old presence-only behaviour instead of silently demoting a working
// GPU to software for the whole daemon run.
func probeHWEncoder(ctx context.Context, ffmpegPath string, hw HWAccel) error {
	pctx, cancel := context.WithTimeout(ctx, hwProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(pctx, ffmpegPath, hwProbeArgs(hw)...)
	winproc.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err == nil || pctx.Err() != nil {
		return nil
	}
	if line := ffmpegFailureLine(string(out)); line != "" {
		return errors.New(line)
	}
	return err
}

// ffmpegFailureLine picks the ffmpeg output line that explains the failure:
// the encoder-open one when present (the final line is usually the generic
// "Nothing was written into output file"), else the last line that is not
// libva's own chatter ("libva info: ..."), printed regardless of -loglevel.
func ffmpegFailureLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, l := range lines {
		if isEncoderInitFailureLine(l) {
			return strings.TrimSpace(l)
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l != "" && !strings.HasPrefix(l, "libva info:") {
			return l
		}
	}
	return ""
}
