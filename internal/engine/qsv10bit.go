package engine

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// QSV 10-bit decode probe. Mirrors the scale_cuda / libplacebo probes: having
// QSV compiled in does NOT prove the 10-bit path WORKS. On some Intel drivers
// (observed: iHD 1.17 on a Jasper Lake-class iGPU, 2026-07-22) the p010
// GPU→system-memory transfer fails outright:
//
//	[AVHWFramesContext] Error synchronizing the operation: -16 (EBUSY)
//	[hevc_qsv] Failed to transfer data to output frame: -1313558101
//	[enc:h264_qsv] Could not open encoder before EOF … exit status 218
//
// ffmpeg then writes ZERO bytes, so seg-0 never lands and the player hangs on
// "Preparando…" until the 60 s mark-ready timeout. Other Intel generations
// (Arc, Tiger Lake+) handle p010 fine, so this must NOT be a blanket rule —
// hard-coding "10-bit ⇒ CPU decode" would silently strip HW decode from every
// healthy host to work around one broken driver. We probe instead.

var (
	qsv10BitCacheMu sync.Mutex
	qsv10BitCache   = map[string]bool{}
)

// hasNonLibvaOutput reports whether ffmpeg emitted anything beyond the libva
// driver banner. The Intel driver writes those lines straight to the fd, so
// `-loglevel error` does not suppress them and a perfectly healthy QSV run is
// never byte-silent:
//
//	libva info: VA-API version 1.17.0
//	libva info: User environment variable requested driver 'iHD'
//	libva info: Trying to open /usr/lib/.../iHD_drv_video.so
//	libva info: Found init function __vaDriverInit_1_17
//	libva info: va_openDriver() returns 0
//
// Measured: a working 8-bit QSV decode emits exactly these and nothing else; the
// broken 10-bit decode adds real diagnostics on top. Anything that is not part
// of the banner counts as a failure signal.
func hasNonLibvaOutput(out []byte) bool {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "libva info:") ||
			strings.HasPrefix(line, "libva error:") ||
			strings.HasPrefix(line, "Trying to open") ||
			strings.HasPrefix(line, "Found init function") ||
			strings.HasPrefix(line, "va_openDriver()") {
			continue
		}
		return true
	}
	return false
}

// writeTempProbeFile encodes a tiny 10-bit HEVC sample with hevc_qsv and returns
// its path. Generated (not shipped) so the probe exercises THIS host's encoder
// and needs no binary asset in the image. Caller must removeProbeFile it.
func writeTempProbeFile(ctx context.Context, ffmpegPath string) (string, error) {
	dir, err := os.MkdirTemp("", "unarr-qsvprobe-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "probe.mp4")
	out, err := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-nostats",
		"-f", "lavfi", "-i", "testsrc2=size=256x256:rate=1:duration=1",
		"-vf", "format=p010le",
		"-c:v", "hevc_qsv", "-frames:v", "2",
		"-y", path,
	).CombinedOutput()
	if err != nil {
		os.RemoveAll(dir)
		if detail := strings.TrimSpace(lastLine(out)); detail != "" {
			return "", fmt.Errorf("%s", detail)
		}
		return "", err
	}
	return path, nil
}

// removeProbeFile deletes the temp dir writeTempProbeFile created.
func removeProbeFile(path string) {
	if path == "" {
		return
	}
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		log.Printf("[qsv] probe cleanup failed for %s: %v", path, err)
	}
}

// FFmpegSupportsQSV10BitDecode reports whether this host can ACTUALLY decode a
// 10-bit source through QSV and hand the frame to a CPU filter chain — the
// exact flow a 10-bit HEVC transcode uses (`-hwaccel qsv
// -hwaccel_output_format nv12` → scale/format on CPU → h264_qsv).
//
// Fails closed: any error → false → the caller decodes 10-bit sources in
// software and keeps the QSV ENCODER (correct output, just more CPU). Cached
// per binary EXCEPT a context timeout, which is transient (busy box) and must
// not pin the slow path for the whole run.
func FFmpegSupportsQSV10BitDecode(ffmpegPath string) bool {
	if ffmpegPath == "" {
		return false
	}
	qsv10BitCacheMu.Lock()
	if v, ok := qsv10BitCache[ffmpegPath]; ok {
		qsv10BitCacheMu.Unlock()
		return v
	}
	qsv10BitCacheMu.Unlock()

	// Probe the REAL failure mode end to end: encode a tiny 10-bit HEVC clip
	// with QSV, then decode it back through `-hwaccel qsv` with the same nv12
	// download the transcode path uses. A filter-only probe would miss it —
	// the break is in the decoder's frame transfer, not in any filter.
	//
	// 20 s covers both passes on a cold/busy box (QSV device init is the slow
	// part, and it happens twice here).
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	tmp, err := writeTempProbeFile(ctx, ffmpegPath)
	if err != nil {
		// Couldn't even produce a 10-bit HEVC sample (no hevc_qsv encoder, no
		// device). Nothing proven about DECODE, but the conservative answer is
		// the same: keep 10-bit sources on the software decoder.
		log.Printf("[qsv] 10-bit decode probe skipped (sample encode failed) — 10-bit sources will decode in software: %v", err)
		// Same rule as the main path: don't cache a transient deadline, or one
		// busy moment at startup would pin every 10-bit source to the software
		// decoder for the whole daemon run.
		if ctx.Err() != context.DeadlineExceeded {
			qsv10BitCacheMu.Lock()
			qsv10BitCache[ffmpegPath] = false
			qsv10BitCacheMu.Unlock()
		}
		return false
	}
	defer removeProbeFile(tmp)

	out, err := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-nostats",
		"-hwaccel", "qsv", "-hwaccel_output_format", "nv12",
		"-i", tmp,
		"-vf", "scale=-2:64,format=yuv420p",
		"-frames:v", "1", "-f", "null", "-",
	).CombinedOutput()
	// A clean exit is NOT sufficient on its own: inside the HLS pipeline this
	// same driver bug was observed reporting the transfer failure and producing
	// zero frames while ffmpeg still exited 0. So require both a clean exit AND
	// no diagnostics — but only after dropping the libva banner, which the Intel
	// driver prints to stderr on EVERY QSV run regardless of -loglevel:
	//   libva info: VA-API version 1.17.0
	//   libva info: Trying to open …/iHD_drv_video.so
	// Measured on a working 8-bit QSV decode: exit 0 + 10 libva lines + ZERO
	// other lines. On the broken 10-bit decode: 10 error lines. Treating raw
	// stderr as the signal would therefore mark every healthy Intel host as
	// broken and silently strip its HW decode — the exact fleet-wide regression
	// this probe exists to avoid.
	supported := err == nil && !hasNonLibvaOutput(out)

	if supported || ctx.Err() != context.DeadlineExceeded {
		qsv10BitCacheMu.Lock()
		qsv10BitCache[ffmpegPath] = supported
		qsv10BitCacheMu.Unlock()
	}
	if supported {
		log.Printf("[qsv] 10-bit HW decode works — 10-bit sources keep hardware decode")
	} else {
		detail := strings.TrimSpace(lastLine(out))
		if detail == "" && err != nil {
			detail = err.Error()
		}
		log.Printf("[qsv] 10-bit HW decode unavailable — 10-bit sources will decode in software (QSV encode kept): %v", detail)
	}
	return supported
}
