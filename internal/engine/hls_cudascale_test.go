package engine

import (
	"strings"
	"testing"
)

// F4: buildHLSFFmpegArgsAt must use the full-GPU scale_cuda path ONLY for an
// SDR NVENC downscale with no burn-in on a host that probed scale_cuda — and
// keep the CPU `scale=` path for every case that needs CPU frames (HDR tonemap,
// burn-in, no downscale, non-NVENC, or scale_cuda unavailable).

func nvencCfg(quality string, burn *int) HLSSessionConfig {
	return HLSSessionConfig{
		SessionID:         "test-cudascale",
		SourcePath:        "/tmp/in.mkv",
		Quality:           quality,
		AudioIndex:        -1,
		BurnSubtitleIndex: burn,
		Transcode: TranscodeRuntime{
			FFmpegPath:    "/usr/bin/ffmpeg",
			HWAccel:       HWAccelNVENC,
			HasScaleCuda:  true,
			HasLibplacebo: true,
			TonemapHDR:    true,
		},
	}
}

func argsFor(cfg HLSSessionConfig, probe *StreamProbe) string {
	return strings.Join(buildHLSFFmpegArgsAt(cfg, probe, "/tmp/tmpdir", 0, 0), " ")
}

func TestCudaScale_SDRDownscale_UsesGPU(t *testing.T) {
	probe := &StreamProbe{Width: 3840, Height: 2160, DurationSec: 100} // SDR (HDR == "")
	got := argsFor(nvencCfg("1080p", nil), probe)
	if !strings.Contains(got, "scale_cuda=-2:1080") {
		t.Errorf("expected scale_cuda for SDR NVENC downscale; got:\n%s", got)
	}
	if !strings.Contains(got, "-hwaccel_output_format cuda") {
		t.Errorf("expected -hwaccel_output_format cuda; got:\n%s", got)
	}
	if strings.Contains(got, "scale=-2:1080") {
		t.Errorf("CPU scale must NOT appear on the cuda path; got:\n%s", got)
	}
}

func TestCudaScale_HDR_StaysOnCPU(t *testing.T) {
	probe := &StreamProbe{Width: 3840, Height: 2160, HDR: "HDR10", DurationSec: 100}
	got := argsFor(nvencCfg("1080p", nil), probe)
	if strings.Contains(got, "scale_cuda") {
		t.Errorf("HDR must NOT use scale_cuda (needs the tonemap on CPU frames); got:\n%s", got)
	}
	if strings.Contains(got, "-hwaccel_output_format cuda") {
		t.Errorf("HDR must NOT pin frames to CUDA; got:\n%s", got)
	}
	if !strings.Contains(got, "libplacebo") {
		t.Errorf("HDR should still tonemap via libplacebo; got:\n%s", got)
	}
}

func TestCudaScale_BurnIn_StaysOnCPU(t *testing.T) {
	idx := 0
	probe := &StreamProbe{Width: 3840, Height: 2160, DurationSec: 100}
	got := argsFor(nvencCfg("1080p", &idx), probe)
	if strings.Contains(got, "scale_cuda") {
		t.Errorf("burn-in requested must NOT use scale_cuda (overlay runs on CPU frames); got:\n%s", got)
	}
}

func TestCudaScale_NoDownscale_StaysOnCPU(t *testing.T) {
	// Source already at/below the cap → no downscale → no point pinning to CUDA.
	probe := &StreamProbe{Width: 1920, Height: 1080, DurationSec: 100}
	got := argsFor(nvencCfg("1080p", nil), probe)
	if strings.Contains(got, "scale_cuda") || strings.Contains(got, "-hwaccel_output_format cuda") {
		t.Errorf("no downscale must NOT use the cuda scale path; got:\n%s", got)
	}
}

func TestCudaScale_ProbeAbsent_StaysOnCPU(t *testing.T) {
	cfg := nvencCfg("1080p", nil)
	cfg.Transcode.HasScaleCuda = false // probe said no / non-CUDA host
	probe := &StreamProbe{Width: 3840, Height: 2160, DurationSec: 100}
	got := argsFor(cfg, probe)
	if strings.Contains(got, "scale_cuda") {
		t.Errorf("scale_cuda unavailable must fall back to CPU scale; got:\n%s", got)
	}
	if !strings.Contains(got, "scale=-2:1080") {
		t.Errorf("expected CPU scale fallback; got:\n%s", got)
	}
}

func TestCudaScale_Software_StaysOnCPU(t *testing.T) {
	cfg := nvencCfg("1080p", nil)
	cfg.Transcode.HWAccel = HWAccelNone // libx264, no CUDA decode
	probe := &StreamProbe{Width: 3840, Height: 2160, DurationSec: 100}
	got := argsFor(cfg, probe)
	if strings.Contains(got, "scale_cuda") || strings.Contains(got, "-hwaccel_output_format cuda") {
		t.Errorf("software encoder must NOT use the cuda scale path; got:\n%s", got)
	}
}

func TestCudaScale_QSV_StaysOnCPU(t *testing.T) {
	// A non-NVENC HW encoder (HW decode, but not h264_nvenc/cuda) must keep the
	// CPU scale — scale_cuda is NVIDIA-only. Distinct from the software case.
	cfg := nvencCfg("1080p", nil)
	cfg.Transcode.HWAccel = HWAccelQSV
	probe := &StreamProbe{Width: 3840, Height: 2160, DurationSec: 100}
	got := argsFor(cfg, probe)
	if strings.Contains(got, "scale_cuda") || strings.Contains(got, "-hwaccel_output_format cuda") {
		t.Errorf("QSV must NOT use the cuda scale path; got:\n%s", got)
	}
}

func TestCudaScale_OriginalQuality_StaysOnCPU(t *testing.T) {
	// "original" → no height cap (maxH == 0) → no downscale → no cuda path.
	probe := &StreamProbe{Width: 3840, Height: 2160, DurationSec: 100}
	got := argsFor(nvencCfg("original", nil), probe)
	if strings.Contains(got, "scale_cuda") || strings.Contains(got, "-hwaccel_output_format cuda") {
		t.Errorf("original quality (no cap) must NOT use the cuda scale path; got:\n%s", got)
	}
}
