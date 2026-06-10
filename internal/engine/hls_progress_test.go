package engine

import (
	"math"
	"testing"
)

func TestParseFFmpegProgress(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		wantSpeed float64
		wantFps   float64
		wantOK    bool
	}{
		{"realtime", "frame=  123 fps= 30 q=28.0 size=  456kB time=00:00:08.00 bitrate=467.0kbits/s speed=1.05x", 1.05, 30, true},
		{"slow", "frame=  12 fps=2.4 q=-1.0 size=  40kB time=00:00:00.40 speed=0.18x", 0.18, 2.4, true},
		{"tight_spacing", "speed=2x", 2, 0, true},
		{"no_speed", "[libplacebo @ 0x55] Spent 2657ms on a slow shader", 0, 0, false},
		{"warning_line", "[hevc @ 0x7f] Could not find ref with POC 12", 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sp, fps, ok := parseFFmpegProgress(c.line)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if math.Abs(sp-c.wantSpeed) > 1e-9 {
				t.Errorf("speed=%v want %v", sp, c.wantSpeed)
			}
			if math.Abs(fps-c.wantFps) > 1e-9 {
				t.Errorf("fps=%v want %v", fps, c.wantFps)
			}
		})
	}
}

func TestIsInputBoundLine(t *testing.T) {
	bound := []string{
		"[http @ 0x55] HTTP error: Connection reset by peer",
		"rw_timeout reached, aborting",
		"Error in the pull function.",
		"tcp://: I/O error",
	}
	for _, l := range bound {
		if !isInputBoundLine(l) {
			t.Errorf("expected input-bound: %q", l)
		}
	}
	notBound := []string{
		"frame= 1 fps=30 speed=1.0x",
		"[libplacebo] slow shader",
	}
	for _, l := range notBound {
		if isInputBoundLine(l) {
			t.Errorf("expected NOT input-bound: %q", l)
		}
	}
}

// hlsStderrCapture must frame on \r (progress) as well as \n (warnings),
// fold progress into the EWMA, and surface a sustained slow encode as < 1.0x.
func TestHlsStderrCaptureProgressEWMA(t *testing.T) {
	s := &HLSSession{}
	s.cfg.SessionID = "test-session-00000000"
	c := &hlsStderrCapture{owner: s}

	// Cold-start frames ffmpeg emits while the pipeline fills — must be skipped
	// (hlsStatsWarmupSkip) so they don't drag the EWMA into a false struggle.
	warmup := "frame=0 fps=0 speed=0.01x\r" +
		"frame=0 fps=0 speed=0.04x\r"
	// A burst of \r-terminated steady-state progress lines, like real ffmpeg.
	chunk := "frame=1 fps=2 speed=0.20x\r" +
		"frame=2 fps=2 speed=0.21x\r" +
		"frame=3 fps=2 speed=0.19x\r" +
		"frame=4 fps=2 speed=0.20x\r" +
		"frame=5 fps=2 speed=0.20x\r"
	if _, err := c.Write([]byte(warmup + chunk)); err != nil {
		t.Fatal(err)
	}
	st := s.GetTranscodeStats()
	// 7 progress lines written, first hlsStatsWarmupSkip(2) discarded → 5 counted.
	if st.Samples != 5 {
		t.Fatalf("samples=%d want 5 (7 lines - 2 warmup)", st.Samples)
	}
	if st.SpeedX > 0.5 || st.SpeedX < 0.1 {
		t.Errorf("speedX EWMA=%v, want ~0.2 (sustained slow encode)", st.SpeedX)
	}
	if st.InputBound {
		t.Error("not input-bound for a pure slow encode")
	}

	// A \n-terminated I/O error line flips input-bound.
	if _, err := c.Write([]byte("tcp://: I/O error\n")); err != nil {
		t.Fatal(err)
	}
	if !s.GetTranscodeStats().InputBound {
		t.Error("expected input-bound after I/O error line")
	}
}
