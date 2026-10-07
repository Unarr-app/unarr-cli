package engine

import (
	"log"
	"path/filepath"
	"strings"
	"time"
)

// HW → software encoder AUTO-FALLBACK.
//
// A hardware encoder that cannot even OPEN fails identically on every relaunch:
// the auto-restart supervisor used to burn its whole budget (3 restarts, each
// refreshing the debrid URL for nothing) against the same broken h264_qsv and
// report transcode_failed. Observed 2026-09-23 on a UGreen NAS whose container
// entrypoint dropped the render group: /dev/dri/renderD128 existed (so
// DetectHWAccel picked QSV) but was unreadable, and every run died with
//
//	[h264_qsv] Error creating a MFX session: -9.
//	[enc:h264_qsv] Could not open encoder before EOF
//
// When ffmpeg exits after reporting one of these encoder-open failures before
// the session produced any output, it relaunches ONCE on libx264. Slower, but
// it plays — which is the only thing the viewer can act on.

// encoderInitFailureMarkers are lowercase stderr fragments that mean the video
// ENCODER could not be initialised (device/runtime missing or unreadable), as
// opposed to a source read error or a mid-stream crash.
//
// Deliberately NOT here: "Could not open encoder before EOF". ffmpeg prints it
// whenever the input ends before a single frame reached the encoder — a debrid
// link reset right after the header or a corrupt file start say it too. Those
// need the normal auto-restart (which refreshes the URL), not a permanent
// switch to libx264.
var encoderInitFailureMarkers = []string{
	"error creating a mfx session",          // QSV: oneVPL found no usable implementation
	"error while opening encoder",           // generic: avcodec_open2 rejected the encoder
	"no capable devices found",              // NVENC: no usable GPU
	"openencodesessionex failed",            // NVENC: driver refused the session
	"cannot load libcuda",                   // NVENC: driver libs absent in the container
	"failed to initialise vaapi connection", // VAAPI: device unreadable / no driver
}

func isEncoderInitFailureLine(line string) bool {
	l := strings.ToLower(line)
	for _, m := range encoderInitFailureMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// markEncoderInitFailed records that the running ffmpeg reported an encoder-open
// failure. Read by fallbackToSoftwareEncode once that ffmpeg exits.
func (s *HLSSession) markEncoderInitFailed() {
	s.mu.Lock()
	s.encoderInitFailed = true
	s.mu.Unlock()
}

// fallbackToSoftwareEncode performs the ONE hardware → libx264 fallback for a
// session whose HW encoder failed to open, and relaunches ffmpeg from target.
// Returns true iff THIS call relaunched (the caller must then not run its own
// auto-restart); false when the session is software already, already fell
// back, is closed, or the failure was not an encoder-open one.
//
// The restart budget and the gaveUp latch are reset like fallbackToTranscode
// does: the software run is a fresh attempt, not a retry of the broken one.
//
// Only a session that never wrote init.mp4 may fall back. Once the HW encoder
// has produced output, the player holds that init.mp4 (its SPS/PPS) and keeps
// it across the uniform playlist, so libx264 segments appended after a
// mid-session failure (e.g. an NVENC seek-restart hitting the consumer-card
// session limit) would decode against the wrong parameter sets. That case stays
// on the normal auto-restart path.
func (s *HLSSession) fallbackToSoftwareEncode(target int) bool {
	s.mu.Lock()
	if s.closed || s.hwFellBack || !s.encoderInitFailed || s.cfg.Transcode.HWAccel == HWAccelNone {
		s.mu.Unlock()
		return false
	}
	if fileExists(filepath.Join(s.tmpDir, "video", "init.mp4")) {
		s.mu.Unlock()
		return false
	}
	s.hwFellBack = true
	s.restartCount = 0
	s.lastRestartAt = time.Time{}
	s.gaveUp = false
	hwCodec := s.cfg.Transcode.HWAccel.FFmpegVideoCodec("h264")
	s.mu.Unlock()

	log.Printf("[hls %s] %s encoder failed to open - falling back to software libx264",
		shortHLSID(s.cfg.SessionID), hwCodec)
	if target < 0 {
		target = 0
	}
	if err := s.restartFromSegment(target); err != nil {
		log.Printf("[hls %s] software fallback relaunch failed: %v", shortHLSID(s.cfg.SessionID), err)
	}
	return true
}

// withoutHW returns the runtime a software-fallback relaunch encodes with: no
// HW encoder, and no HW-only capability that the arg builder would otherwise
// key off (scale_cuda, QSV 10-bit decode). libplacebo is already gated on
// HWAccel != none by the arg builder.
func (t TranscodeRuntime) withoutHW() TranscodeRuntime {
	t.HWAccel = HWAccelNone
	t.HasScaleCuda = false
	t.HasQSV10BitDecode = false
	return t
}
