package engine

import (
	"context"
	"os/exec"
	"testing"
)

func TestBenchmarkMaxTranscodeHeight_HardwareSkipsProbe(t *testing.T) {
	// Hardware encoders return 2160 without touching ffmpeg — pass a bogus path
	// to prove no subprocess runs.
	for _, hw := range []HWAccel{HWAccelNVENC, HWAccelQSV, HWAccelVAAPI, HWAccelVideoToolbox} {
		got := BenchmarkMaxTranscodeHeight(context.Background(), "/nonexistent/ffmpeg", hw)
		if got != 2160 {
			t.Errorf("hw=%s: got %d, want 2160", hw, got)
		}
	}
}

func TestBenchmarkMaxTranscodeHeight_NoFFmpegKeepsDefault(t *testing.T) {
	if got := BenchmarkMaxTranscodeHeight(context.Background(), "", HWAccelNone); got != 1080 {
		t.Errorf("empty ffmpeg path: got %d, want 1080 (historical default)", got)
	}
}

func TestBenchmarkMaxTranscodeHeight_SoftwareReturnsValidRung(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH — software benchmark needs a real encoder")
	}
	got := BenchmarkMaxTranscodeHeight(context.Background(), ffmpeg, HWAccelNone)
	switch got {
	case 1080, 720, 480:
		// any rung is valid; the exact one depends on the host's CPU.
	default:
		t.Errorf("software ceiling = %d, want one of {1080,720,480}", got)
	}
}

func TestMeasureEncodeRealtimeFactor_RealEncoder(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	factor, ok := measureEncodeRealtimeFactor(context.Background(), ffmpeg, benchmarkRung{height: 480, width: 854})
	if !ok {
		t.Fatal("480p probe failed to run on a host with ffmpeg")
	}
	if factor <= 0 {
		t.Errorf("realtime factor = %.2f, want > 0", factor)
	}
}
