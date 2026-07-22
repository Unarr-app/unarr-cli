package engine

import "testing"

// The QSV 10-bit probe decides whether a host keeps hardware decode for every
// 10-bit source it ever plays, and the answer is cached for the daemon's life.
// A false NEGATIVE silently costs a healthy host its HW decode — the exact
// fleet-wide regression the probe exists to prevent — so the "did it work"
// signal must tolerate the libva banner the Intel driver always prints.
//
// Both fixtures below are verbatim stderr captured from the affected box
// (iHD 1.17): the healthy 8-bit decode and the broken 10-bit one.

const libvaBanner = `libva info: VA-API version 1.17.0
libva info: User environment variable requested driver 'iHD'
libva info: Trying to open /usr/lib/x86_64-linux-gnu/dri/iHD_drv_video.so
libva info: Found init function __vaDriverInit_1_17
libva info: va_openDriver() returns 0
libva info: VA-API version 1.17.0
libva info: User environment variable requested driver 'iHD'
libva info: Trying to open /usr/lib/x86_64-linux-gnu/dri/iHD_drv_video.so
libva info: Found init function __vaDriverInit_1_17
libva info: va_openDriver() returns 0`

const brokenDecodeOutput = libvaBanner + `
[AVHWFramesContext @ 0x7f36cc00e580] Error synchronizing the operation: -16
[hevc_qsv @ 0x55c970ea9bc0] Failed to transfer data to output frame: -1313558101.
[vist#0:0/hevc @ 0x55c970e93080] [dec:hevc_qsv @ 0x55c970eaecc0] Error while processing the decoded data`

func TestHasNonLibvaOutput_HealthyRunIsClean(t *testing.T) {
	if hasNonLibvaOutput([]byte(libvaBanner)) {
		t.Error("the libva banner alone must NOT count as failure — a working QSV decode " +
			"always prints it, and treating it as noise would strip HW decode from every healthy Intel host")
	}
}

func TestHasNonLibvaOutput_EmptyIsClean(t *testing.T) {
	if hasNonLibvaOutput(nil) {
		t.Error("no output at all must not count as failure")
	}
	if hasNonLibvaOutput([]byte("   \n\n  ")) {
		t.Error("whitespace-only output must not count as failure")
	}
}

func TestHasNonLibvaOutput_DetectsRealDriverFailure(t *testing.T) {
	if !hasNonLibvaOutput([]byte(brokenDecodeOutput)) {
		t.Error("the p010 transfer failure must be detected through the libva banner")
	}
}
