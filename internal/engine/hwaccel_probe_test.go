package engine

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/testutil"
)

func TestHWProbeArgs(t *testing.T) {
	qsv := hwProbeArgs(HWAccelQSV)
	if !slices.Contains(qsv, "h264_qsv") || slices.Contains(qsv, "-vaapi_device") {
		t.Errorf("qsv probe argv: %v", qsv)
	}
	vaapi := strings.Join(hwProbeArgs(HWAccelVAAPI), " ")
	for _, want := range []string{"-vaapi_device /dev/dri/renderD128", "format=nv12,hwupload", "-c:v h264_vaapi"} {
		if !strings.Contains(vaapi, want) {
			t.Errorf("vaapi probe argv missing %q: %s", want, vaapi)
		}
	}
}

func TestFFmpegFailureLinePrefersTheEncoderOpenLine(t *testing.T) {
	out := `libva info: VA-API version 1.17.0
[h264_qsv @ 0x1] Error creating a MFX session: -9.
[vost#0:0/h264_qsv @ 0x2] Terminating thread with error: Invalid argument
[out#0/null @ 0x3] Nothing was written into output file, because at least one of its streams received no packets.
`
	if got := ffmpegFailureLine(out); got != "[h264_qsv @ 0x1] Error creating a MFX session: -9." {
		t.Errorf("got %q", got)
	}
	if got := ffmpegFailureLine("boom\nlibva info: x\n"); got != "boom" {
		t.Errorf("libva chatter must be skipped, got %q", got)
	}
}

func TestProbeHWEncoder(t *testing.T) {
	testutil.RequireShellStubs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ok := stubTool(t, "ffmpeg", "exit 0\n")
	if err := probeHWEncoder(ctx, ok, HWAccelQSV); err != nil {
		t.Errorf("working encoder reported as broken: %v", err)
	}

	broken := stubTool(t, "ffmpeg", `echo "[h264_qsv @ 0x1] Error creating a MFX session: -9." >&2
echo "[out#0/null @ 0x3] Nothing was written into output file" >&2
exit 171
`)
	err := probeHWEncoder(ctx, broken, HWAccelQSV)
	if err == nil || !strings.Contains(err.Error(), "MFX session") {
		t.Errorf("broken encoder must fail with the MFX line, got %v", err)
	}
}
