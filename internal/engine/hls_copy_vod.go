// Package engine implements exact, seekable H.264 COPY-VOD HLS. MP4 sample
// tables / Matroska Cues provide real boundaries for local and ranged remote
// sources. Each requested segment copies video without decoding, trims seek
// preroll by GOP membership, and places audio on one continuous timeline.
// No full-file materialisation is needed. Unsupported/unindexed sources retain
// the continuous HLS path; fabricated uniform VOD boundaries are never used.

package engine

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

// copyVODTargetSec is the nominal segment length for COPY-VOD. Larger than the
// encode mode's 2 s (segmentDurationFor) because every segment is a separate
// ffmpeg spawn — 6 s keeps the spawn count for a 2 h film near ~1200 instead of
// ~3600, while staying within Apple's recommended 6 s target. Actual durations
// vary: each segment runs from one source keyframe to the next group boundary.
const copyVODTargetSec = 6.0

// copyVODSegExt is the on-disk + playlist extension for COPY-VOD segments.
// MPEG-TS (.ts), not fMP4 (.m4s) — see the package comment.
const copyVODSegExt = ".ts"

// planCopySegments turns a sorted keyframe list + total duration into the
// segment boundary table: starts[i]..starts[i+1] is segment i. starts[0] is
// always 0 and the final element is always duration, so len(starts)-1 ==
// segment count. Every interior boundary is a real keyframe, so an on-demand
// `-ss starts[i] -c copy` lands exactly (no mid-GOP cut).
//
// Greedy grouping: open a new segment at the first keyframe that is at least
// copyVODTargetSec past the current segment's start. A trailing sliver shorter
// than ~1 s is folded into the previous segment (a sub-1 s fragment is a
// needless extra spawn + a seekbar speck).
func planCopySegments(keyframes []float64, duration float64) []float64 {
	starts := []float64{0}
	last := 0.0
	for _, kf := range keyframes {
		// Skip keyframes at/below the current start (incl. the first ~0 one) and
		// anything at/after duration (a final-frame keyframe makes no segment).
		if kf <= last+0.001 || kf >= duration-0.001 {
			continue
		}
		if kf-last >= copyVODTargetSec {
			starts = append(starts, kf)
			last = kf
		}
	}
	// Close the table at the true duration. Fold a sub-1 s tail back into the
	// previous segment so we never list a near-empty final fragment.
	if duration-last < 1.0 && len(starts) > 1 {
		starts[len(starts)-1] = duration
	} else {
		starts = append(starts, duration)
	}
	return starts
}

// renderVideoPlaylistCopyVOD builds the complete VOD media playlist for a
// COPY-VOD session: every segment listed, exact per-segment EXTINF from the
// keyframe boundary table, ENDLIST present from the first fetch. The player
// learns the full timeline + total duration immediately and can seek anywhere.
// MPEG-TS segments → no EXT-X-MAP (no init), HLS version 3.
func renderVideoPlaylistCopyVOD(starts []float64) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	// TARGETDURATION must be >= the longest segment, rounded up. Segments are
	// keyframe-bounded so they can exceed the nominal target; compute the max.
	maxDur := 0.0
	for i := 0; i+1 < len(starts); i++ {
		if d := starts[i+1] - starts[i]; d > maxDur {
			maxDur = d
		}
	}
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int(maxDur)+1))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	for i := 0; i+1 < len(starts); i++ {
		b.WriteString(fmt.Sprintf("#EXTINF:%.6f,\n", starts[i+1]-starts[i]))
		b.WriteString(fmt.Sprintf("seg-%d%s\n", i, copyVODSegExt))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// startCopyVOD attempts to set up a COPY-VOD session: plan the segment table,
// render the complete VOD manifest (full duration + seek-anywhere), and — for a
// remote source — kick off a one-shot subtitle sidecar extractor. Returns false
// (no error) if the source can't be COPY-VOD'd (non-H.264 codec, no known
// duration, remote without HTTP range support, local keyframe-index failure) so
// the caller falls back to the legacy EVENT copy path. No video ffmpeg is
// spawned here — segments are produced lazily on first request.
func startCopyVOD(ctx context.Context, s *HLSSession) bool {
	// Cast-targeted sessions must use fMP4 (the Default Media Receiver plays
	// fMP4 HLS, not mpegts). Skip the MPEG-TS copy-vod path → fall back to the
	// fMP4 EVENT-copy (buildHLSCopyArgs).
	if s.cfg.Fmp4Only {
		log.Printf("[hls %s] copy-vod skipped: Fmp4Only (cast) - using fMP4 EVENT copy",
			shortHLSID(s.cfg.SessionID))
		return false
	}
	// MPEG-TS transport carries H.264 universally but not HEVC/AV1 (see package
	// comment). Non-H.264 copy → legacy EVENT path (no regression).
	if !mediainfo.CopyVODEligibleCodec(s.probe.VideoCodec) {
		log.Printf("[hls %s] copy-vod skipped: codec %q not TS-eligible - using EVENT copy",
			shortHLSID(s.cfg.SessionID), s.probe.VideoCodec)
		return false
	}

	if s.durationSec <= 0 || s.cfg.sourceRef() == "" {
		return false
	}
	// Proxy first: the index + IDR reads below warm the very header/seek-index
	// blocks every later segment spawn would otherwise re-download.
	s.startCopySourceProxy()
	if s.cfg.SingleConnection && s.copyProxy == nil {
		// One-connection source (IPTV): only the proxy's single upstream link
		// keeps the index, IDR probe, segment spawns and subtitle windows on ONE
		// provider connection. Without it they would each open their own.
		log.Printf("[hls %s] copy-vod skipped: single-connection source without the source proxy - using EVENT copy",
			shortHLSID(s.cfg.SessionID))
		return false
	}
	starts, ok := planCopyVOD(ctx, s)
	if !ok {
		s.stopCopySourceProxy() // the fallback paths read the source directly
		s.copyProxy = nil
		return false
	}
	s.copyVOD = true
	s.copySegStarts = starts
	s.segmentCount = len(starts) - 1
	s.manifestVideo = renderVideoPlaylistCopyVOD(starts)
	s.manifestRoot = renderMasterPlaylistCopy(s.probe)

	// Every boundary is exact. Produce only requested segments; seeking far
	// ahead never waits for a linear pass or consumes a film's worth of disk.
	s.copyLazy = true
	s.copyCtx, s.copyCancel = context.WithCancel(context.Background())
	// Two segment spawns may run at once — except on a one-connection source,
	// where they would only fight over the single upstream link (each jump
	// between their offsets reopens it); there one runs at a time.
	slots := 2
	if s.cfg.SingleConnection {
		slots = 1
	}
	s.copySlots = make(chan struct{}, slots)
	s.copyWake = make(chan struct{}, 1)
	s.copyWG.Add(1) // session not published yet: no Close can race this Add
	go s.runCopyPrefetch()
	s.readyMu.Lock()
	s.readyMax = s.segmentCount
	s.exited = true
	s.readyMu.Unlock()
	if s.cfg.SourceURL != "" {
		startCopyVODSubtitles(s)
	}
	mode := "exact/on-demand"

	log.Printf("[hls %s] copy-vod: %d segments, %.1fs (%s)",
		shortHLSID(s.cfg.SessionID), s.segmentCount, s.durationSec, mode)
	return true
}

// planCopyVOD builds the exact segment table and checks every cut is an IDR.
// ok=false means COPY-VOD must be declined (s.copyNeedsEncode says why).
func planCopyVOD(ctx context.Context, s *HLSSession) (starts []float64, ok bool) {
	starts, _, ok = indexKeyframesFast(ctx, s, s.copySource())
	if !ok {
		return nil, false
	}
	if len(starts) < 2 {
		log.Printf("[hls %s] copy-vod planning yielded no segments - using EVENT copy",
			shortHLSID(s.cfg.SessionID))
		return nil, false
	}
	if !copySourceHasIDR(ctx, s, starts) {
		log.Printf("[hls %s] copy-vod requires IDR cuts; using seekable video encode", shortHLSID(s.cfg.SessionID))
		s.copyNeedsEncode = true
		return nil, false
	}
	return starts, true
}

// startCopyVODSubtitles starts extracting a WebVTT sidecar per TEXT subtitle
// track (served as subs/s<idx>.vtt), mirroring the EVENT copy path's in-pass
// sidecars — needed because COPY-VOD's on-demand segments never read the whole
// file. Each generated segment's cues are extracted right after it, from the
// bytes the source proxy still has cached; see hls_subtitle_timeslices.go. No-op
// when the source has no text subtitles.
//
// This used to be a second download of the WHOLE file next to playback (MKV
// interleaves cues with the video, so mapping fewer tracks saves no bytes),
// which on a bandwidth-bound link starved segment generation outright: 19 s of
// video played per 80 s of wall clock.
//
// Caller must have set copyCtx + copySegStarts and not published the session.
func startCopyVODSubtitles(s *HLSSession) {
	if len(s.textSubtitleTracks()) == 0 {
		return
	}
	tag := fmt.Sprintf("[hls %s]", shortHLSID(s.cfg.SessionID))
	s.subWin = newSubtitleWindows(tag, s.copySegStarts, s.extractSubtitleWindow)
	s.subsDone = make(chan struct{})
	s.copyWG.Add(1)
	go func() {
		defer s.copyWG.Done()
		defer close(s.subsDone)
		s.subWin.run(s.copyCtx)
	}()
}
