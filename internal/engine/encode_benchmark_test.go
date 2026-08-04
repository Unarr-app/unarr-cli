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

// The wrapper must stay a thin view over the core: whatever the core decides,
// the daemon's ceiling is exactly that number and nothing is re-derived.
func TestMeasureEncodeCeiling_AgreesWithWrapper(t *testing.T) {
	cases := []struct {
		name       string
		ffmpeg     string
		hw         HWAccel
		wantReason string
	}{
		{"hardware", "/nonexistent/ffmpeg", HWAccelNVENC, EncodeReasonHardware},
		{"no ffmpeg", "", HWAccelNone, EncodeReasonNoFFmpeg},
		{"probe cannot run", "/nonexistent/ffmpeg", HWAccelNone, EncodeReasonUnmeasurable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := MeasureEncodeCeiling(context.Background(), tc.ffmpeg, tc.hw)
			if res.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", res.Reason, tc.wantReason)
			}
			if got := BenchmarkMaxTranscodeHeight(context.Background(), tc.ffmpeg, tc.hw); got != res.Ceiling {
				t.Errorf("wrapper ceiling %d != core ceiling %d", got, res.Ceiling)
			}
		})
	}
}

// A rung whose probe never ran reports Measured=false, so a renderer can't
// print "0.0x realtime" as if it were a measurement.
func TestMeasureEncodeCeiling_FailedProbesAreNotMeasurements(t *testing.T) {
	res := MeasureEncodeCeiling(context.Background(), "/nonexistent/ffmpeg", HWAccelNone)
	if len(res.Rungs) != len(softwareBenchmarkRungs) {
		t.Fatalf("rungs = %d, want %d", len(res.Rungs), len(softwareBenchmarkRungs))
	}
	for _, r := range res.Rungs {
		if r.Measured {
			t.Errorf("%dp reported as measured with no ffmpeg", r.Height)
		}
	}
	if res.Ceiling != 1080 {
		t.Errorf("ceiling = %d, want the 1080 default when nothing could be measured", res.Ceiling)
	}
}

func TestMeasureEncodeCeiling_SoftwareRecordsEvidence(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH — software benchmark needs a real encoder")
	}
	res := MeasureEncodeCeiling(context.Background(), ffmpeg, HWAccelNone)
	if len(res.Rungs) == 0 {
		t.Fatal("no rungs recorded")
	}
	if res.Threshold != realtimeMarginSoftware {
		t.Errorf("threshold = %v, want %v", res.Threshold, realtimeMarginSoftware)
	}
	first := res.Rungs[0]
	if first.Measured && first.Factor <= 0 {
		t.Errorf("measured rung has factor %v, want > 0", first.Factor)
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
