package engine

import (
	"strings"
	"testing"
)

// Regression: QSV transcodes produced ZERO bytes and exit 218, so seg-0 never
// landed and the player spun on "Preparando…" until the 60 s mark-ready timeout
// (2026-07-22: 4.5% of transcode sessions on a QSV agent ever reached ready, vs
// 82.7% for copy sessions on the same agent). Two stacked defects:
//
//  1. Omitting -hwaccel_output_format does NOT mean "frames on CPU" for QSV the
//     way it does for VAAPI — ffmpeg defaults it to `qsv`, so the decoder emits
//     GPU surfaces into our pure-CPU filter chain and ffmpeg can't bridge them:
//     Impossible to convert between the formats supported by the filter
//     'graph -1 input from stream 0:0' (src: qsv) and 'auto_scale_0'
//     Error reinitializing filters! ... -38 (Function not implemented)
//  2. Pinning the download format instead trips an iHD driver bug on 10-bit
//     (p010) surfaces — the GPU→system transfer itself fails:
//     [AVHWFramesContext] Error synchronizing the operation: -16 (EBUSY)
//     [hevc_qsv] Failed to transfer data to output frame: -1313558101
//
// Measured on the affected box with the real 10-bit HEVC file: HW decode → 0
// bytes; software decode + QSV encode → 680 KB in 4 s. 8-bit HW decode verified
// working there.
//
// Defect (1) is universal to QSV, so nv12 is pinned unconditionally. Defect (2)
// is DRIVER-SPECIFIC — Arc / Tiger Lake+ decode p010 fine — so it hangs off the
// FFmpegSupportsQSV10BitDecode probe rather than a blanket "10-bit ⇒ CPU"
// rule that would strip HW decode from healthy hosts. The QSV ENCODER is kept
// in every case: it was never the broken part.

func qsvCfg(quality string, burn *int) HLSSessionConfig {
	return HLSSessionConfig{
		SessionID:         "test-qsv",
		SourcePath:        "/tmp/in.mkv",
		Quality:           quality,
		AudioIndex:        -1,
		BurnSubtitleIndex: burn,
		Transcode: TranscodeRuntime{
			FFmpegPath: "/usr/bin/ffmpeg",
			HWAccel:    HWAccelQSV,
			TonemapHDR: true,
		},
	}
}

// 8-bit: keep HW decode, but pin nv12 so the CPU filter chain gets system frames.
func TestQSV_8Bit_HWDecodePinnedToNV12(t *testing.T) {
	probe := &StreamProbe{Width: 3840, Height: 2160, BitDepth: 8, DurationSec: 100}
	got := argsFor(qsvCfg("1080p", nil), probe)

	if !strings.Contains(got, "-hwaccel qsv") {
		t.Errorf("8-bit QSV should keep HW-accelerated decode; got:\n%s", got)
	}
	if !strings.Contains(got, "-hwaccel_output_format nv12") {
		t.Errorf("QSV must download frames to system memory for the CPU filter chain "+
			"(without this ffmpeg defaults to `qsv` surfaces and the encode dies at exit 218); got:\n%s", got)
	}
	if !strings.Contains(got, "scale=-2:1080") {
		t.Errorf("expected CPU scale for the QSV downscale; got:\n%s", got)
	}
}

// 10-bit on a host whose probe FAILED (the incident box: HEVC Main 10 →
// p010 transfer bug → zero bytes): drop to a software decoder. The QSV ENCODER
// must survive — dropping that too would silently cost the user their hardware
// acceleration entirely.
func TestQSV_10Bit_BrokenProbe_FallsBackToSoftwareDecode(t *testing.T) {
	probe := &StreamProbe{Width: 3840, Height: 2160, BitDepth: 10, DurationSec: 100}
	got := argsFor(qsvCfg("1080p", nil), probe) // HasQSV10BitDecode false

	if strings.Contains(got, "-hwaccel qsv") {
		t.Errorf("10-bit QSV must decode on the CPU when the probe failed; got:\n%s", got)
	}
	if strings.Contains(got, "-hwaccel_output_format") {
		t.Errorf("no -hwaccel means no output-format pin; got:\n%s", got)
	}
	if !strings.Contains(got, "h264_qsv") {
		t.Errorf("the QSV ENCODER must be kept for 10-bit sources; got:\n%s", got)
	}
}

// The regression guard that matters for the FLEET: a host whose QSV handles
// 10-bit fine (Arc, Tiger Lake+) must KEEP hardware decode. A blanket
// "10-bit ⇒ CPU decode" rule would silently strip HW decode from these hosts to
// work around one broken driver.
func TestQSV_10Bit_WorkingProbe_KeepsHWDecode(t *testing.T) {
	cfg := qsvCfg("1080p", nil)
	cfg.Transcode.HasQSV10BitDecode = true
	probe := &StreamProbe{Width: 3840, Height: 2160, BitDepth: 10, DurationSec: 100}
	got := argsFor(cfg, probe)

	if !strings.Contains(got, "-hwaccel qsv") {
		t.Errorf("a host with working 10-bit QSV must keep HW decode; got:\n%s", got)
	}
	if !strings.Contains(got, "-hwaccel_output_format nv12") {
		t.Errorf("HW decode still needs the nv12 pin for the CPU filter chain; got:\n%s", got)
	}
}

// HDR sources are 10-bit in practice → same probe-driven decision.
func TestQSV_HDR_BrokenProbe_FallsBackToSoftwareDecode(t *testing.T) {
	probe := &StreamProbe{Width: 3840, Height: 2160, BitDepth: 10, HDR: "HDR10", DurationSec: 100}
	got := argsFor(qsvCfg("1080p", nil), probe)

	if strings.Contains(got, "-hwaccel qsv") {
		t.Errorf("10-bit HDR QSV must decode on the CPU when the probe failed; got:\n%s", got)
	}
}

// ffprobe does not always report bits_per_raw_sample, so BitDepth is "0 if
// unknown" while HDR is still populated. Gating on BitDepth alone would send
// such a file down the HW path and reproduce the original zero-byte failure —
// DecideAction already treats these as one class (tenBitOrHDR).
func TestQSV_HDRWithUnknownBitDepth_FallsBackToSoftwareDecode(t *testing.T) {
	probe := &StreamProbe{Width: 3840, Height: 2160, BitDepth: 0, HDR: "HDR10", DurationSec: 100}
	got := argsFor(qsvCfg("1080p", nil), probe)

	if strings.Contains(got, "-hwaccel qsv") {
		t.Errorf("an HDR source with unreported bit depth must decode on the CPU; got:\n%s", got)
	}
}

// Burn-in composites on CPU frames (scale2ref+overlay) — an 8-bit source still
// needs the nv12 pin for that chain to receive usable frames.
func TestQSV_BurnIn_8Bit_PinsSystemMemory(t *testing.T) {
	burn := 0
	probe := &StreamProbe{
		Width: 1920, Height: 1080, BitDepth: 8, DurationSec: 100,
		SubtitleTracks: []ProbeSubtitleTrack{{Codec: "hdmv_pgs_subtitle"}},
	}
	got := argsFor(qsvCfg("1080p", &burn), probe)

	if !strings.Contains(got, "-hwaccel_output_format nv12") {
		t.Errorf("QSV burn-in path must keep frames on CPU; got:\n%s", got)
	}
}

// Guard the neighbours: only QSV gets the nv12 pin. VAAPI relies on omitting the
// flag entirely (its decoder does hand back CPU frames), and software has no
// -hwaccel at all — pinning either would be a regression.
func TestNonQSV_DoesNotGetNV12Pin(t *testing.T) {
	probe := &StreamProbe{Width: 3840, Height: 2160, BitDepth: 8, DurationSec: 100}

	vaapi := qsvCfg("1080p", nil)
	vaapi.Transcode.HWAccel = HWAccelVAAPI
	if got := argsFor(vaapi, probe); strings.Contains(got, "-hwaccel_output_format") {
		t.Errorf("VAAPI must not pin an output format; got:\n%s", got)
	}

	soft := qsvCfg("1080p", nil)
	soft.Transcode.HWAccel = HWAccelNone
	if got := argsFor(soft, probe); strings.Contains(got, "-hwaccel_output_format") {
		t.Errorf("software path must not pin an output format; got:\n%s", got)
	}
}
