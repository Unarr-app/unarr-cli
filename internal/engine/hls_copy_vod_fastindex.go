// Package engine — hls_copy_vod_fastindex.go: the two-phase keyframe index that
// keeps a COPY-VOD session from stalling on a cold sidecar miss.
//
// The full keyframe index is a whole-file demux. Measured cold over NFS that is
// 153 s for a 12 GB h264 and >180 s for a 6 GB dense-GOP telesync — far too long
// to hold up session start, and the reason a miss used to end in EVENT copy,
// which ignores the resume position outright (a resume at 59 min played from 0).
//
// Rather than choose between "block the player" and "lose the resume point",
// start from a WINDOW: ffprobe `-read_intervals` around the resume position
// returns real keyframes for that region in ~7 s (measured), enough to plan a
// seekable playlist immediately. The exact whole-file index then finishes in the
// background and lands in the sidecar, so the next play — and every seek outside
// the window on this one — is instant.
//
// The window's timestamps are a true subset of the full index (verified against
// one), so segments planned from it cut on real keyframes: no GOP-overlap echo
// like the uniform-segment fallback would produce on a local source.
package engine

import (
	"context"
	"errors"
	"log"
	"math"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

// copyVODWindowSec is how much of the file the fast path indexes around the
// resume position. Wide enough to plan many segments ahead of the player (at
// copyVODTargetSec each, 600 s is ~100 segments of runway) while staying a
// bounded read.
const copyVODWindowSec = 600.0

// copyVODWindowLeadSec backs the window up before the resume position so the
// segment containing it has a keyframe boundary at or before it, and a small
// backwards seek stays inside the indexed region.
const copyVODWindowLeadSec = 30.0

// backgroundKeyframeIndexTimeout bounds the detached full index. Generous: it
// competes with nothing on the critical path and only fills the sidecar.
const backgroundKeyframeIndexTimeout = 30 * time.Minute

// copyVODViable reports whether COPY-VOD is structurally possible for this
// session, judged only on facts known before any I/O: the Cast fMP4 requirement
// and the H.264-only MPEG-TS transport gate. These are the two reasons
// startCopyVOD declines that no amount of indexing can change (its other refusals
// — a failed index, a remote source without range support — are discovered later
// and are handled by the fallback inside the VideoCopy branch).
//
// Kept separate from startCopyVOD so the copy-vs-transcode question can be
// settled before cache placement, without running the index as a side effect.
func copyVODViable(cfg HLSSessionConfig, probe *StreamProbe) bool {
	if cfg.Fmp4Only {
		return false // Cast: Default Media Receiver plays fMP4, not mpegts
	}
	if probe == nil {
		return false
	}
	return mediainfo.CopyVODEligibleCodec(probe.VideoCodec)
}

// resumeTranscodeThresholdSec is how far into a file a resume must sit before it
// is worth trading `-c:v copy` for a re-encode. Below this the EVENT copy remux
// reaches the position almost immediately (it runs at I/O speed, ~19× realtime),
// so paying for a transcode would be the worse deal.
const resumeTranscodeThresholdSec = 120.0

// shouldTranscodeForResume reports whether a session that has just been refused
// COPY-VOD should transcode rather than fall back to EVENT copy.
//
// EVENT copy always produces from t=0 and cannot be `-ss`'d (an offset tfdt under
// an EVENT playlist breaks iOS's native parser — see buildHLSCopyArgs), so a
// resume deep into a long file means the viewer waits for the linear remux to
// reach it. Transcoding costs CPU but seeks exactly.
//
// Requires a known duration and a resume that is genuinely inside the file: a
// stale position at or past the end is handled by the startIdx logic (start from
// 0), and re-encoding for it would be pure waste.
func shouldTranscodeForResume(cfg HLSSessionConfig, probe *StreamProbe) bool {
	if cfg.StartSec <= resumeTranscodeThresholdSec {
		return false
	}
	if probe == nil || probe.DurationSec <= 0 {
		return false
	}
	return cfg.StartSec < probe.DurationSec
}

// planWindowedCopySegments builds a segment table that is keyframe-exact inside
// the indexed window and uniform outside it.
//
// Outside the window we have no keyframe data, so those boundaries are wall-clock
// multiples — the same trade-off REMOTE sources already accept (planUniformSegments):
// an on-demand `-ss` there rounds down to the preceding keyframe, so seek is
// GOP-accurate rather than exact. Inside the window every boundary is a real
// keyframe. The player gets the full timeline and can seek anywhere immediately;
// the background index upgrades the whole file to exact on the next play.
//
// Returns nil when the window yields nothing usable, so the caller can fall back.
func planWindowedCopySegments(window []float64, duration float64) []float64 {
	if duration <= 0 || len(window) == 0 {
		return nil
	}
	winStart, winEnd := window[0], window[len(window)-1]

	starts := []float64{0}
	last := 0.0
	appendBoundary := func(t float64) {
		// Keep the table strictly increasing and clear of the duration edge; a
		// sub-target step would produce a needless extra segment.
		if t-last >= copyVODTargetSec && t < duration-0.001 {
			starts = append(starts, t)
			last = t
		}
	}

	// Uniform boundaries BEFORE the window. Stop short of winStart by a full
	// target: otherwise the last uniform boundary can land within
	// copyVODTargetSec of the window's first keyframe, and appendBoundary would
	// then drop that keyframe — losing the exact cut the window was read for.
	for t := copyVODTargetSec; t <= winStart-copyVODTargetSec; t += copyVODTargetSec {
		appendBoundary(t)
	}
	// Exact keyframe boundaries inside the window. These are the ones that matter:
	// the resume position sits here, and each is a real keyframe.
	for _, kf := range window {
		appendBoundary(kf)
	}
	// Uniform again past the window, on the ORIGINAL wall-clock grid rather than
	// stepping from winEnd. Segments here are cut by a per-segment `-ss` that
	// rounds down to the preceding keyframe, and keeping the grid aligned with the
	// pre-window one means a later exact index produces a comparable table.
	for t := math.Ceil(winEnd/copyVODTargetSec) * copyVODTargetSec; t < duration; t += copyVODTargetSec {
		appendBoundary(t)
	}

	if duration-last < 1.0 && len(starts) > 1 {
		starts[len(starts)-1] = duration
	} else {
		starts = append(starts, duration)
	}
	if len(starts) < 2 {
		return nil
	}
	return starts
}

// indexKeyframesFast returns a keyframe table for a local source without paying
// the whole-file demux up front.
//
// exact reports whether every boundary in the returned table is a real keyframe
// (a full index, from the sidecar or a fast enough live index) or whether the
// table is windowed — keyframe-exact only around startSec. ok=false means the
// caller should fall back to EVENT copy.
//
// On a windowed result it detaches a background full index that writes the
// sidecar, so this cost is paid once per file rather than once per play.
func indexKeyframesFast(ctx context.Context, s *HLSSession, src string) (starts []float64, exact bool, ok bool) {
	if kfs, hit := mediainfo.ReadCachedKeyframes(src); hit {
		log.Printf("[hls %s] copy-vod keyframe index: sidecar hit (%d keyframes)",
			shortHLSID(s.cfg.SessionID), len(kfs))
		return planCopySegments(kfs, s.durationSec), true, true
	}

	ffprobe := s.cfg.Transcode.FFprobePath
	from := s.cfg.StartSec - copyVODWindowLeadSec
	if from < 0 {
		from = 0
	}
	t0 := time.Now()
	window, werr := mediainfo.IndexKeyframeWindow(ctx, ffprobe, src, from, copyVODWindowSec)
	if werr == nil {
		if starts := planWindowedCopySegments(window, s.durationSec); starts != nil {
			log.Printf("[hls %s] copy-vod keyframe index: window %.0f..%.0fs in %s (%d keyframes) - exact seek around resume, full index running in background",
				shortHLSID(s.cfg.SessionID), from, from+copyVODWindowSec, time.Since(t0).Round(time.Millisecond), len(window))
			startBackgroundKeyframeIndex(ffprobe, src, s.cfg.SessionID)
			return starts, false, true
		}
	} else {
		// A window that TIMED OUT means the mount is too slow right now. The full
		// index reads strictly more of the same file, so running it inline would
		// block session start for many minutes — worse than the fallback it is
		// meant to avoid. Bail to EVENT copy and let the background index (started
		// below) fix the next play instead.
		if errors.Is(werr, mediainfo.ErrKeyframeIndexTimeout) {
			log.Printf("[hls %s] copy-vod keyframe window timed out (%v) - using EVENT copy, indexing in background",
				shortHLSID(s.cfg.SessionID), werr)
			startBackgroundKeyframeIndex(ffprobe, src, s.cfg.SessionID)
			return nil, false, false
		}
		log.Printf("[hls %s] copy-vod keyframe window failed (%v) - trying full index",
			shortHLSID(s.cfg.SessionID), werr)
	}

	// No usable window for a structural reason (a short file, or ffprobe rejected
	// the interval) rather than slowness: the full index is affordable here, and
	// the alternative is EVENT copy with no resume at all.
	kfs, err := mediainfo.IndexKeyframes(ctx, ffprobe, src)
	if err != nil {
		log.Printf("[hls %s] copy-vod keyframe index failed (%v) - using EVENT copy",
			shortHLSID(s.cfg.SessionID), err)
		return nil, false, false
	}
	if werr := mediainfo.WriteCachedKeyframes(src, kfs); werr != nil {
		log.Printf("[hls %s] copy-vod keyframe sidecar write skipped: %v",
			shortHLSID(s.cfg.SessionID), werr)
	}
	return planCopySegments(kfs, s.durationSec), true, true
}

// bgKeyframeIndex tracks which sources already have a background index running.
// Without it, a viewer who starts and stops a session repeatedly (or two devices
// opening the same film) would stack one full-container NFS read per attempt —
// PrewarmKeyframes' own freshness check only skips work already FINISHED, so it
// does not dedup concurrent runs.
var bgKeyframeIndex = struct {
	sync.Mutex
	inFlight map[string]struct{}
}{inFlight: make(map[string]struct{})}

// startBackgroundKeyframeIndex runs the full index detached from the session and
// caches it. Deliberately on context.Background(): the point is to outlive this
// session so the NEXT play gets an exact table. Best-effort — a failure just
// means the next play windows again.
func startBackgroundKeyframeIndex(ffprobePath, src, sessionID string) {
	bgKeyframeIndex.Lock()
	if _, running := bgKeyframeIndex.inFlight[src]; running {
		bgKeyframeIndex.Unlock()
		return
	}
	bgKeyframeIndex.inFlight[src] = struct{}{}
	bgKeyframeIndex.Unlock()

	go func() {
		defer func() {
			bgKeyframeIndex.Lock()
			delete(bgKeyframeIndex.inFlight, src)
			bgKeyframeIndex.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), backgroundKeyframeIndexTimeout)
		defer cancel()
		t0 := time.Now()
		if err := mediainfo.PrewarmKeyframes(ctx, ffprobePath, src); err != nil {
			log.Printf("[hls %s] background keyframe index failed after %s: %v",
				shortHLSID(sessionID), time.Since(t0).Round(time.Second), err)
			return
		}
		log.Printf("[hls %s] background keyframe index cached in %s - next play seeks exactly",
			shortHLSID(sessionID), time.Since(t0).Round(time.Second))
	}()
}
