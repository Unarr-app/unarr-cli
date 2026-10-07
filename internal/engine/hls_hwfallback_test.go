package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/testutil"
)

func TestIsEncoderInitFailureLine(t *testing.T) {
	hits := []string{
		// Verbatim from the UGreen NAS whose container lost the render group.
		"[h264_qsv @ 0x556bd5d9e840] Error creating a MFX session: -9.",
		"[vost#0:0/h264_qsv @ 0x1] [enc:h264_qsv @ 0x2] Error while opening encoder - maybe incorrect parameters such as bit_rate, rate, width or height.",
		"[h264_nvenc @ 0x1] OpenEncodeSessionEx failed: unsupported device (2): (no details)",
		"[h264_nvenc @ 0x1] No capable devices found",
		"[h264_nvenc @ 0x1] Cannot load libcuda.so.1",
		"[AVHWDeviceContext @ 0x1] Failed to initialise VAAPI connection: -1 (unknown libva error).",
	}
	for _, l := range hits {
		if !isEncoderInitFailureLine(l) {
			t.Errorf("should flag encoder-open failure: %q", l)
		}
	}
	misses := []string{
		// Also printed when the INPUT ends before any frame reached the encoder
		// (debrid reset after the header) — that needs a URL-refreshing restart.
		"[vost#0:0/h264_qsv @ 0x556bd5bb56c0] [enc:h264_qsv @ 0x556bd5a181c0] Could not open encoder before EOF",
		"[in#0/matroska,webm @ 0x1] Could not find codec parameters for stream 18 (Attachment: none): unknown codec",
		"[vost#0:0/h264_qsv @ 0x1] Discarding stream metadata 'BPS' because the stream is being re-encoded.",
		"[https @ 0x1] Connection reset by peer",
		"[out#0/hls @ 0x1] Nothing was written into output file, because at least one of its streams received no packets.",
	}
	for _, l := range misses {
		if isEncoderInitFailureLine(l) {
			t.Errorf("must not flag unrelated line: %q", l)
		}
	}
}

func TestWithoutHWDropsEveryHWOnlyCapability(t *testing.T) {
	in := TranscodeRuntime{
		FFmpegPath: "/usr/bin/ffmpeg", HWAccel: HWAccelQSV, Preset: "veryfast",
		HasScaleCuda: true, HasQSV10BitDecode: true, HasLibplacebo: true, TonemapHDR: true,
	}
	got := in.withoutHW()
	if got.HWAccel != HWAccelNone || got.HasScaleCuda || got.HasQSV10BitDecode {
		t.Errorf("HW capabilities survived: %+v", got)
	}
	if got.FFmpegPath != in.FFmpegPath || got.Preset != in.Preset || !got.TonemapHDR {
		t.Errorf("non-HW settings must be kept: %+v", got)
	}
	if in.HWAccel != HWAccelQSV {
		t.Error("withoutHW must not mutate the receiver's source value")
	}
}

func TestFallbackToSoftwareEncodeGuards(t *testing.T) {
	cases := []struct {
		name  string
		setup func(s *HLSSession)
	}{
		{"no encoder-open failure seen", func(s *HLSSession) { s.encoderInitFailed = false }},
		{"already software", func(s *HLSSession) { s.cfg.Transcode.HWAccel = HWAccelNone }},
		{"already fell back", func(s *HLSSession) { s.hwFellBack = true }},
		{"session closed", func(s *HLSSession) { s.closed = true }},
		// The HW encoder already produced output: the player holds its init.mp4,
		// so libx264 segments appended now would decode with the wrong SPS/PPS.
		{"hw output already produced", func(s *HLSSession) {
			if err := os.MkdirAll(filepath.Join(s.tmpDir, "video"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(s.tmpDir, "video", "init.mp4"), []byte("ftyp"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &HLSSession{encoderInitFailed: true, tmpDir: t.TempDir()}
			s.cfg.Transcode.HWAccel = HWAccelQSV
			tc.setup(s)
			if s.fallbackToSoftwareEncode(0) {
				t.Fatal("fallback must not fire")
			}
		})
	}
}

// The relaunch after an encoder-open failure must run libx264, not the broken
// HW encoder again, and must happen only once per session.
func TestFallbackToSoftwareEncodeRelaunchesOnLibx264Once(t *testing.T) {
	testutil.RequireShellStubs(t)
	argsFile := filepath.Join(t.TempDir(), "args")
	ffmpeg := stubTool(t, "ffmpeg", `echo "$@" > "`+argsFile+`"`+"\n")

	s := &HLSSession{
		tmpDir:            t.TempDir(),
		probe:             &StreamProbe{DurationSec: 60, Width: 1920, Height: 1080},
		segmentCount:      30,
		durationSec:       60,
		encoderInitFailed: true,
		restartCount:      3,
		gaveUp:            true,
		exited:            true,
	}
	s.cfg.SessionID = "hwfallback-test"
	s.cfg.Transcode = TranscodeRuntime{FFmpegPath: ffmpeg, HWAccel: HWAccelQSV, HasQSV10BitDecode: true}
	t.Cleanup(func() { _ = s.Close() })

	if !s.fallbackToSoftwareEncode(0) {
		t.Fatal("expected the one-shot fallback to fire")
	}
	args := waitForFile(t, argsFile)
	if !strings.Contains(args, "libx264") || strings.Contains(args, "qsv") {
		t.Errorf("relaunch must be pure software, got argv: %s", args)
	}
	s.mu.Lock()
	restarts, gaveUp := s.restartCount, s.gaveUp
	s.mu.Unlock()
	if restarts != 0 || gaveUp {
		t.Errorf("fallback must start a fresh budget: restartCount=%d gaveUp=%v", restarts, gaveUp)
	}
	if s.fallbackToSoftwareEncode(0) {
		t.Error("second fallback must be a no-op")
	}
}

func waitForFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never written", path)
	return ""
}
