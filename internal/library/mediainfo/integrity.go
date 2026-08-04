package mediainfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// Deep truncation detection (checks A/B/C). The base ffprobe (ExtractMediaInfo)
// only reads the CONTAINER HEADER, so it sees the file's *claimed* duration —
// which stays intact when a download is truncated (the moov/Cues/Info that hold
// the duration live near the START of the file). A half-downloaded 24-min anime
// episode therefore probes as a healthy 24-min file and plays for ~7 min before
// silently ending; nothing warns the user. These checks close that gap AT SCAN
// TIME by comparing the header's claim against what's actually in the file:
//
//   B — TAIL GAP (primary): demux the last window of packets and take the max
//       presentation timestamp. If it falls far short of the header duration,
//       the tail data is missing → truncated. Catches the anime case above.
//   A — SIZE SHORTFALL (gate): the file is far smaller than bit_rate × duration
//       implies. Never flags on its own (VBR / missing bit_rate make it noisy);
//       it only gates the decode confirm below.
//   C — TAIL DECODE (confirm): when A says the bytes are short yet B says the
//       tail packets reach the end (byte holes rather than a clean cut), decode
//       a few frames near the end. A decode error confirms tail corruption.
//       Only ever ADDS a verdict — never suppresses B — so its flakiness on
//       seek-past-EOF can't cause a false "healthy".
//
// All are LOCAL-file only (a remote/debrid URL is probed by a different path).

const (
	// B: flag when header_duration - last_tail_pts exceeds max(floor, frac·dur).
	truncGapFloorSec = 30.0
	truncGapFrac     = 0.03
	// Tail read window: start the demux this far before the claimed end. Must
	// comfortably exceed the gap threshold so a HEALTHY file's tail read still
	// reaches ~header_duration (gap ≈ 0).
	truncTailFloorSec = 90.0
	truncTailFrac     = 0.06
	// A: file smaller than this fraction of bit_rate × duration ⇒ shortfall.
	truncSizeRatio = 0.85
)

// truncDecodeTimeout bounds the tail decode (check C). See truncProbeTimeout for
// why both are 60s. A var, not a const, only so tests can shorten it.
var truncDecodeTimeout = 60 * time.Second

// truncProbeTimeout bounds a single tail demux.
//
// 60s, not 30s: measured on a real NFS library (2026-07-21), a tail demux of a
// 4K remote file takes 8-47s with NO concurrency at all, and 7-107s with the
// default Workers=8. At 30s every one of those healthy files came back "signal:
// killed" — 70/70 tail-probe failures in the fleet sample were our own timeout,
// not a single real corruption. The cap still exists to bound a genuinely hung
// mount; files that exceed it go to the deferred serial retry
// (library.retryPendingTails), which re-probes them without contention.
//
// A var, not a const, only so tests can shorten it; nothing in production
// reassigns it.
var truncProbeTimeout = 60 * time.Second

// ErrProbeExpired marks a deep probe that ran out of time rather than reaching
// a verdict. It is strictly a scheduling signal: the file is NOT damaged and
// NOT known-healthy — it simply was not checked, so the caller can re-queue it
// under less pressure. Never map this to a damaged verdict.
var ErrProbeExpired = errors.New("probe expired")

// AssessTruncation runs the deep (post-header) truncation checks on a LOCAL video
// file whose header already probed OK. Returns a "damaged" verdict only on a
// corroborated signal, else nil. headerDur is the container's claimed duration
// (seconds); ffmpegPath may be "" → the decode confirm (C) is skipped.
//
// Conservative by construction: it only flags when the file's own header
// contradicts its contents, so a healthy file is never marked damaged.
//
// The error return is ONLY a scheduling signal, never a verdict: ErrProbeExpired
// means the tail demux ran out of time (slow/contended storage) so the file was
// left UNCHECKED and deserves a deferred retry. Callers that ignore the error
// keep exactly the old behaviour — nil verdict, nothing flagged.
func AssessTruncation(ctx context.Context, ffprobePath, ffmpegPath, filePath string, headerDur float64) (*IntegrityInfo, error) {
	if headerDur <= 0 || strings.Contains(filePath, "://") {
		return nil, nil
	}
	fi, err := os.Stat(filePath)
	if err != nil {
		return nil, nil
	}
	fileSize := fi.Size()

	tailStart := headerDur - tailWindowSec(headerDur)
	if tailStart < 0 {
		tailStart = 0
	}
	lastPTS, bitRate, expired, ok := probeTail(ctx, ffprobePath, filePath, tailStart)
	if !ok {
		// Couldn't demux the tail → don't guess. Only a genuine timeout is worth
		// re-queueing (measured 107s→0.8s for the same file once probed
		// serially); a missing ffprobe or an unreadable container would fail the
		// same way again, so those stay a plain no-verdict.
		if expired {
			return nil, ErrProbeExpired
		}
		return nil, nil
	}

	// C (tail decode) is lazy: only the byte-short-but-full-duration branch needs
	// it, so pass a thunk the pure verdict runs at most once. Nil when ffmpeg is
	// unavailable — that branch then declines to flag.
	//
	// decodeExpired records a decode that ran out of time. tailDecodeFails fails
	// OPEN (an inconclusive run reports "no failure"), which is what keeps a slow
	// box from inventing damage — but on its own that makes a timed-out decode
	// indistinguishable from a clean one, so the file would never be re-checked.
	// Tracking it lets the caller re-queue instead of silently calling it healthy.
	var decodeConfirm func() bool
	decodeExpired := false
	if ffmpegPath != "" {
		decodeConfirm = func() bool {
			failed, expired := tailDecodeFails(ctx, ffmpegPath, filePath, headerDur)
			decodeExpired = expired
			return failed
		}
	}
	verdict := truncationVerdict(headerDur, lastPTS, bitRate, fileSize, decodeConfirm)
	if verdict == nil && decodeExpired {
		return nil, ErrProbeExpired
	}
	return verdict, nil
}

// truncationVerdict is the pure decision core (no I/O), factored out so the
// A/B/C thresholds are unit-testable without invoking ffprobe/ffmpeg.
//
//	headerDur  — container's claimed duration (s)
//	lastPTS    — max presentation timestamp found in the tail demux (s)
//	bitRate    — header overall bit_rate (bits/s), 0 when the container omits it
//	fileSize   — file size in bytes
//	decodeFail — check C: reports whether a tail decode fails; nil = unavailable
func truncationVerdict(headerDur, lastPTS float64, bitRate, fileSize int64, decodeFail func() bool) *IntegrityInfo {
	gap := headerDur - lastPTS
	gapThreshold := math.Max(truncGapFloorSec, truncGapFrac*headerDur)

	// B: the tail data stops well before the claimed end → truncated download.
	if gap > gapThreshold {
		return &IntegrityInfo{Damaged: true, Reason: "truncated"}
	}

	// A: size shortfall — only meaningful when the header carries a bit_rate.
	shortfall := false
	if bitRate > 0 {
		expectedBytes := float64(bitRate) / 8.0 * headerDur
		shortfall = float64(fileSize) < truncSizeRatio*expectedBytes
	}

	// B is clean (packets reach the end) but A says the file is far too small for
	// that duration — packets exist yet bytes are missing (holey / corrupt tail).
	// Confirm with a real decode near the end (C) before flagging.
	if shortfall && decodeFail != nil && decodeFail() {
		return &IntegrityInfo{Damaged: true, Reason: "tail_corrupt"}
	}

	return nil
}

// tailWindowSec is how far before the claimed end the tail demux starts.
func tailWindowSec(headerDur float64) float64 {
	return math.Max(truncTailFloorSec, truncTailFrac*headerDur)
}

// tailProbeOutput is the subset of ffprobe JSON the tail read needs.
type tailProbeOutput struct {
	Packets []struct {
		PTSTime string `json:"pts_time"`
	} `json:"packets"`
	Format struct {
		BitRate string `json:"bit_rate"`
	} `json:"format"`
}

// probeTail demuxes video packets from startSec to EOF and returns the maximum
// presentation timestamp seen plus the header's overall bit_rate. `-read_intervals`
// keeps this a bounded tail read (seek + demux the last window), not a full-file
// pass. ok is false only when ffprobe itself failed to run; expired then says
// whether that failure was our own per-probe deadline (retryable on quieter
// storage) rather than a cancelled scan or a probe that could never run.
func probeTail(parent context.Context, ffprobePath, filePath string, startSec float64) (lastPTS float64, bitRate int64, expired, ok bool) {
	ctx, cancel := context.WithTimeout(parent, truncProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "error",
		"-select_streams", "v:0",
		// "START%" (empty end) = from startSec to end of file.
		"-read_intervals", fmt.Sprintf("%.3f%%", startSec),
		"-show_entries", "format=bit_rate:packet=pts_time",
		"-of", "json",
		filePath,
	)
	winproc.HideWindow(cmd)
	// Killing ffprobe on deadline is not enough: Output() keeps waiting on the
	// inherited pipes, so a probe stuck on a hung NFS mount held a scan worker
	// long past its timeout (the test for this took the full sleep before
	// WaitDelay was added). Cap that grace so the deadline is real.
	cmd.WaitDelay = 5 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// Surface why the tail demux failed (timeout vs bad -read_intervals vs
		// unreadable container) instead of a silent skip — matches ExtractMediaInfo
		// / indexKeyframes, which both include ffprobe's stderr on failure.
		//
		// Name our own deadline explicitly: a killed ffprobe writes nothing to
		// stderr, so the old line read "tail probe failed: signal: killed ()" and
		// was indistinguishable from a real read error. That ambiguity is exactly
		// what made 70 healthy 4K files look like corruption in the fleet sample.
		switch {
		case parent.Err() != nil:
			// The whole scan is going down; re-queueing would pile up work nobody
			// will run.
			log.Printf("[integrity] tail probe cancelled for %s (scan shutting down)", filePath)
		case ctx.Err() != nil:
			// Our own deadline: the storage was too slow under load. Worth retrying
			// serially — measured 107s→0.8s for the same file on NFS.
			log.Printf("[integrity] tail probe timed out for %s after %s - file left UNCHECKED, queued for a serial retry",
				filePath, truncProbeTimeout)
			return 0, 0, true, false
		default:
			// ffprobe couldn't start, or the container is unreadable. A retry would
			// fail identically, so don't queue one.
			log.Printf("[integrity] tail probe failed for %s: %v (%s)", filePath, err, strings.TrimSpace(stderr.String()))
		}
		return 0, 0, false, false
	}

	var data tailProbeOutput
	if err := json.Unmarshal(out, &data); err != nil {
		return 0, 0, false, false
	}
	if br, err := strconv.ParseInt(strings.TrimSpace(data.Format.BitRate), 10, 64); err == nil && br > 0 {
		bitRate = br
	}
	for _, p := range data.Packets {
		if v := parseDuration(p.PTSTime); v > lastPTS {
			lastPTS = v
		}
	}
	// A successful run with zero tail packets means the seek landed past all real
	// data — the file ends well before its claimed duration. lastPTS stays 0, so
	// the caller's gap check flags it.
	return lastPTS, bitRate, false, true
}

// tailDecodeFails decodes a few frames near the claimed end. A genuine decode
// error (ffmpeg ran and choked on the bitstream at that offset) confirms the
// tail is broken. It FAILS OPEN — an inconclusive run returns false — so it only
// ever CONFIRMS damage, never health, mirroring probeTail's don't-guess contract.
//
// Failing open matters because this runs inside the scan's worker pool: with
// Workers=8 each file can spawn ffprobe + probeTail + this decode, so on a
// modest box a HEALTHY file's decode can hit the timeout. Treating that
// timeout/cancellation (or a can't-start error) as "corrupt" would wrongly mark
// good files damaged, so those paths return false; only an ffmpeg process that
// actually ran and reported an error counts.
// The second return value reports that the decode ran out of time rather than
// reaching a conclusion; it never affects the verdict (which stays fail-open),
// it only lets the caller re-queue the file for a quieter retry.
func tailDecodeFails(parent context.Context, ffmpegPath, filePath string, headerDur float64) (failed, expired bool) {
	ctx, cancel := context.WithTimeout(parent, truncDecodeTimeout)
	defer cancel()

	seek := headerDur - 10
	if seek < 0 {
		seek = 0
	}
	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-v", "error",
		"-ss", fmt.Sprintf("%.3f", seek),
		"-i", filePath,
		"-frames:v", "3",
		"-f", "null", "-",
	)
	winproc.HideWindow(cmd)
	cmd.WaitDelay = 5 * time.Second // see probeTail: bound the post-kill pipe wait
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()

	// Timed out / cancelled under load → inconclusive, don't flag. Our own
	// deadline expiring is retryable (the storage was busy); a cancelled scan is
	// not, since nobody would run the retry.
	if ctx.Err() != nil {
		return false, parent.Err() == nil
	}
	// A non-ExitError (ffmpeg couldn't start, I/O error) means it never really
	// ran the decode → inconclusive, don't flag. Not retryable: it would fail
	// the same way again.
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		log.Printf("[integrity] tail decode inconclusive for %s: %v", filePath, err)
		return false, false
	}
	// ffmpeg ran: a non-zero exit or an error logged to stderr = real decode
	// failure at the tail.
	return err != nil || strings.Contains(strings.ToLower(stderr.String()), "error"), false
}
