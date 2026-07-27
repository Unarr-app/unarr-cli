// Package engine — hls_copy_vod.go implements the COPY-VOD streaming model.
//
// The legacy VideoCopy path (buildHLSCopyArgs) runs ONE continuous ffmpeg
// `-c:v copy` writing an EVENT playlist that GROWS as ffmpeg outruns playback.
// Two problems for the viewer:
//   - the playlist's duration is unknown until ENDLIST → the seekbar total
//     keeps climbing ("1:23 / 5:10" then "1:48 / 20:55");
//   - you can't seek past the produced region → jumping to minute 50 means
//     waiting for the linear remux to reach it.
//
// COPY-VOD fixes both, Plex/Jellyfin-style, WITHOUT re-encoding video:
//   1. Index the source's keyframes once (ffprobe, key-frames only).
//   2. Group them into ~copyVODTargetSec segments, each STARTING on a keyframe
//      (copy can only cut at keyframes). Render a COMPLETE VOD playlist upfront
//      — every segment listed, real total duration, full seekbar from t=0.
//   3. Transcode each segment ON DEMAND when the player requests it
//      (`ffmpeg -copyts -ss start -i src -to end -c:v copy -f mpegts`),
//      keyframe-aligned. Seeking to minute 50 = generating one ~6 s segment
//      (~100 ms for copy), not waiting out a linear remux.
//
// Transport is MPEG-TS, NOT fMP4. fMP4 needs a single shared EXT-X-MAP init,
// but ffmpeg bakes the `-ss` start offset into each segment's init as an edit
// list (elst) — so a shared init mis-places every segment but the first (the
// player clamps them all to t=0; verified empirically). MPEG-TS segments are
// self-contained: each carries absolute PTS and no init, so independently-cut
// `-c copy` segments concatenate seamlessly across players (hls.js transmux +
// Safari native). The trade-off is codec reach: TS reliably carries H.264 +
// AAC/AC3 across browsers, but NOT HEVC (Apple HLS mandates fMP4 for HEVC) or
// AV1. So COPY-VOD is gated to H.264 sources; HEVC/AV1 copy (only chosen when
// the device declares native decode, i.e. Safari) stays on the legacy EVENT
// path — no regression, just no seek-anywhere there yet.
//
// Scope: H.264 copy sessions. LOCAL files get an exact keyframe index
// (frame-accurate seek). REMOTE sources (connector/IPTV/debrid) can't be
// keyframe-indexed without downloading the whole file, so they plan UNIFORM
// segments from the known duration instead — full duration + seek still work,
// but seek is GOP-rounded (the on-demand `-ss` input-seek lands on the nearest
// keyframe ≤ the boundary). A remote source without HTTP range support, or with
// no known duration, falls back to the legacy EVENT path (see StartHLSSession).
// Remote COPY-VOD also spawns a one-shot subtitle sidecar extractor, since its
// on-demand video segments never read the whole file the way the EVENT copy does.

package engine

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
	"github.com/Unarr-app/unarr-cli/internal/winproc"
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
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", starts[i+1]-starts[i]))
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
		log.Printf("[hls %s] copy-vod skipped: Fmp4Only (cast) — using fMP4 EVENT copy",
			shortHLSID(s.cfg.SessionID))
		return false
	}
	// MPEG-TS transport carries H.264 universally but not HEVC/AV1 (see package
	// comment). Non-H.264 copy → legacy EVENT path (no regression).
	if !mediainfo.CopyVODEligibleCodec(s.probe.VideoCodec) {
		log.Printf("[hls %s] copy-vod skipped: codec %q not TS-eligible — using EVENT copy",
			shortHLSID(s.cfg.SessionID), s.probe.VideoCodec)
		return false
	}

	var starts []float64
	switch {
	case s.cfg.SourceURL != "":
		// REMOTE (connector/IPTV/debrid): a keyframe index would download the
		// whole file, so plan UNIFORM segments from the known duration. The
		// on-demand `-ss` input-seek rounds DOWN to the nearest keyframe, so seek
		// is GOP-accurate (≤copyVODTargetSec off) while the full timeline shows
		// upfront. Needs a known duration + HTTP range support (else every
		// segment's -ss would re-read from byte 0); without either, EVENT copy.
		if s.durationSec <= 0 {
			log.Printf("[hls %s] copy-vod skipped: remote source has no known duration — using EVENT copy",
				shortHLSID(s.cfg.SessionID))
			return false
		}
		if !sourceSupportsRange(ctx, s.cfg.sourceRef()) {
			log.Printf("[hls %s] copy-vod skipped: remote source lacks HTTP range support — using EVENT copy",
				shortHLSID(s.cfg.SessionID))
			return false
		}
		starts = planUniformSegments(s.durationSec)
		// The on-demand video segments never read the whole file, so subtitles
		// ride a separate one-shot pass that fills subs/ progressively.
		startCopyVODSubtitles(s)
	case s.cfg.SourcePath != "":
		// LOCAL: exact keyframe boundaries (frame-accurate seek, no GOP rounding).
		// Prefer the scan-time sidecar (.unarr/<file>.copyseg.json) so playback
		// skips the full demux read; on a miss, index now AND warm the sidecar so
		// the next play (and a re-scan) is instant.
		src := s.cfg.sourceRef()
		kfs, ok := mediainfo.ReadCachedKeyframes(src)
		if ok {
			log.Printf("[hls %s] copy-vod keyframe index: sidecar hit (%d keyframes)",
				shortHLSID(s.cfg.SessionID), len(kfs))
		} else {
			var err error
			kfs, err = mediainfo.IndexKeyframes(ctx, s.cfg.Transcode.FFprobePath, src)
			if err != nil {
				log.Printf("[hls %s] copy-vod keyframe index failed (%v) — using EVENT copy",
					shortHLSID(s.cfg.SessionID), err)
				return false
			}
			if werr := mediainfo.WriteCachedKeyframes(src, kfs); werr != nil {
				log.Printf("[hls %s] copy-vod keyframe sidecar write skipped: %v",
					shortHLSID(s.cfg.SessionID), werr)
			}
		}
		starts = planCopySegments(kfs, s.durationSec)
	default:
		return false
	}

	if len(starts) < 2 {
		log.Printf("[hls %s] copy-vod planning yielded no segments — using EVENT copy",
			shortHLSID(s.cfg.SessionID))
		return false
	}
	s.copyVOD = true
	s.copySegStarts = starts
	s.segmentCount = len(starts) - 1
	s.manifestVideo = renderVideoPlaylistCopyVOD(starts)
	s.manifestRoot = renderMasterPlaylistCopy(s.probe)

	// LOCAL files run a single background segment-muxer PASS: it reads the file
	// linearly (never seeks) and cuts at the exact keyframe boundaries, so
	// segments are contiguous with zero overlap — the echo-free path. Handlers
	// then wait on readyMax like encode mode.
	//
	// REMOTE/uniform sources, and a local file without disk headroom for the
	// whole-file .ts materialisation, stay LAZY: each segment is generated on
	// demand by a per-index `-ss` spawn (GOP-overlap echo present on scene-cut
	// sources, but it plays and doesn't download the whole remote file).
	mode := "remote/uniform lazy"
	if s.cfg.SourcePath != "" && launchCopyVODPass(s) {
		mode = "local/keyframe pass"
	} else {
		s.copyLazy = true
		// No background writer → mark every segment "ready" immediately so the
		// "Preparando…" overlay flips and the player fetches; each fetch then
		// generates its segment on demand.
		s.readyMu.Lock()
		s.readyMax = s.segmentCount
		s.exited = true
		s.readyMu.Unlock()
	}
	log.Printf("[hls %s] copy-vod: %d segments, %.1fs (%s)",
		shortHLSID(s.cfg.SessionID), s.segmentCount, s.durationSec, mode)
	return true
}

// planUniformSegments plans a COPY-VOD segment table at fixed copyVODTargetSec
// boundaries across 0..duration — for REMOTE sources where a real keyframe index
// isn't affordable. Same shape as planCopySegments (starts[0]==0, final element
// ==duration, len-1 == segment count); the difference is the interior boundaries
// are wall-clock multiples, not keyframes, so an on-demand `-ss` input-seek
// rounds down to the nearest preceding keyframe. A sub-1 s trailing sliver is
// folded into the last segment (no near-empty final fragment).
func planUniformSegments(duration float64) []float64 {
	if duration <= 0 {
		return nil
	}
	starts := []float64{0}
	for t := copyVODTargetSec; t < duration-1.0; t += copyVODTargetSec {
		starts = append(starts, t)
	}
	starts = append(starts, duration)
	return starts
}

// sourceSupportsRange reports whether url answers an HTTP byte-range request
// (status 206). COPY-VOD on a remote source seeks every segment with `-ss`,
// which only stays cheap when the server honours Range — otherwise each segment
// re-reads from byte 0. A tight timeout keeps a dead/slow panel from stalling
// session start; any error reports false (→ caller uses the EVENT copy path).
func sourceSupportsRange(ctx context.Context, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Range", "bytes=0-1")
	// IPTV panels commonly gate on a player UA; match a VLC-class client so the
	// probe reflects what the segment ffmpeg can actually pull, not a Go default.
	req.Header.Set("User-Agent", "VLC/3.0.20 LibVLC/3.0.20")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusPartialContent
}

// startCopyVODSubtitles spawns a background ffmpeg that reads the remote source
// ONCE and writes a WebVTT sidecar per TEXT subtitle track (subs/s<idx>.vtt),
// mirroring the EVENT copy path's in-pass sidecars — needed because COPY-VOD's
// on-demand segments never read the whole file. `-flush_packets 1` streams each
// cue to disk so the sidecar fills progressively (ServeSubtitleVTT serves what's
// read so far). The extractor starts a few seconds late so the first video
// segment isn't contended on a single-line panel, and its cancel is stored on
// s.cancel so Close() kills it. No-op when the source has no text subtitles.
func startCopyVODSubtitles(s *HLSSession) {
	var outs []string
	for _, sb := range s.probe.SubtitleTracks {
		if !sb.IsTextSubtitle() {
			continue
		}
		outs = append(outs,
			"-map", fmt.Sprintf("0:s:%d?", sb.Index),
			"-c:s", "webvtt",
			"-flush_packets", "1",
			"-f", "webvtt",
			filepath.Join(s.tmpDir, "subs", fmt.Sprintf("s%d.vtt", sb.Index)),
		)
	}
	if len(outs) == 0 {
		return
	}
	args := []string{
		"-y", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "5",
		"-rw_timeout", "30000000",
		"-i", s.cfg.sourceRef(),
	}
	args = append(args, outs...)

	ffCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	go func() {
		// Yield the panel to the first video segment before opening a second read.
		select {
		case <-ffCtx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		cmd := exec.CommandContext(ffCtx, s.cfg.Transcode.FFmpegPath, args...)
		winproc.HideWindow(cmd)
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil && ffCtx.Err() == nil {
			log.Printf("[hls %s] copy-vod subtitle extractor: %v (%s)",
				shortHLSID(s.cfg.SessionID), err, strings.TrimSpace(errBuf.String()))
			return
		}
		if ffCtx.Err() == nil {
			log.Printf("[hls %s] copy-vod subtitle sidecars complete", shortHLSID(s.cfg.SessionID))
		}
	}()
}

// copyVODPassDiskReserve is the free space to keep AFTER the background pass
// materialises every segment (~source size) into the session tmpdir. Below it,
// startCopyVOD degrades to lazy per-segment generation rather than risk a full disk.
const copyVODPassDiskReserve = 512 * 1024 * 1024

// copyVODPassMaxRestarts bounds restart-from-0 attempts when the pass ffmpeg dies
// unexpectedly. The pass is deterministic and the segment muxer overwrites the
// same seg-N.ts filenames, so a restart re-produces byte-identical segments.
const copyVODPassMaxRestarts = 2

// launchCopyVODPass starts the background segment-muxer pass for a LOCAL source.
// It returns false (caller falls back to lazy per-segment generation) when the
// source can't be stat'd or the disk can't hold the materialised segments. On
// success it resets readyMax=0/exited=false and spawns the pass + poller, so
// handlers block on readyMax exactly like encode mode.
func launchCopyVODPass(s *HLSSession) bool {
	src := s.cfg.sourceRef()
	fi, err := os.Stat(src)
	if err != nil {
		// Can't size the source → can't run the disk guard (CheckDiskSpace with
		// needBytes<=0 no-ops, defeating it) and segmentWaitTimeout would fall to
		// the 60s encode default instead of the size-derived ceiling. Fall back to
		// lazy per-segment generation, which stat-verifies each segment itself.
		log.Printf("[hls %s] copy-vod pass skipped (stat source: %v) — lazy per-segment",
			shortHLSID(s.cfg.SessionID), err)
		return false
	}
	srcSize := fi.Size()
	if err := CheckDiskSpace(s.tmpDir, srcSize, copyVODPassDiskReserve); err != nil {
		log.Printf("[hls %s] copy-vod pass skipped (%v) — lazy per-segment",
			shortHLSID(s.cfg.SessionID), err)
		return false
	}

	args := buildCopyVODPassArgs(s.cfg, s.probe, s.copySegStarts, s.tmpDir)
	ffCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ffCtx, s.cfg.Transcode.FFmpegPath, args...)
	winproc.HideWindow(cmd)
	errBuf := &bytes.Buffer{}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		cancel()
		log.Printf("[hls %s] copy-vod pass start failed (%v) — lazy per-segment",
			shortHLSID(s.cfg.SessionID), err)
		return false
	}

	// Construction-time (session not yet registered / no handlers): plain writes.
	s.cancel = cancel
	s.srcSizeBytes = srcSize
	s.readyMu.Lock()
	s.readyMax = 0
	s.exited = false
	s.readyMu.Unlock()

	go s.waitCopyPass(cmd, ffCtx, errBuf)
	go s.pollSegments(ffCtx)
	return true
}

// waitCopyPass reaps the background segment-muxer pass. On an unexpected failure
// (not a Close-triggered cancel) it restarts from 0 a bounded number of times —
// the pass is deterministic, so a restart re-produces identical segments. When it
// finally stops it marks the session exited so waitForSegment unblocks (nil on a
// clean finish, error on give-up); pollSegments then seals the final segment.
func (s *HLSSession) waitCopyPass(cmd *exec.Cmd, ffCtx context.Context, errBuf *bytes.Buffer) {
	err := cmd.Wait()
	for attempt := 1; err != nil && ffCtx.Err() == nil && attempt <= copyVODPassMaxRestarts; attempt++ {
		log.Printf("[hls %s] copy-vod pass failed (%v: %s) — restart %d/%d from 0",
			shortHLSID(s.cfg.SessionID), err, strings.TrimSpace(errBuf.String()), attempt, copyVODPassMaxRestarts)
		// Restart-from-0 truncates+rewrites every seg-N.ts IN PLACE (segment
		// muxer, no .tmp+rename). Roll the watermark back to 0 so no handler
		// serves a segment the restarted pass is overwriting; pollSegments
		// re-advances it as the segments reappear. Mirrors restartFromSegment.
		s.readyMu.Lock()
		s.readyMax = 0
		s.readyMu.Unlock()
		errBuf.Reset()
		args := buildCopyVODPassArgs(s.cfg, s.probe, s.copySegStarts, s.tmpDir)
		cmd = exec.CommandContext(ffCtx, s.cfg.Transcode.FFmpegPath, args...)
		winproc.HideWindow(cmd)
		cmd.Stderr = errBuf
		if serr := cmd.Start(); serr != nil {
			err = serr
			break
		}
		err = cmd.Wait()
	}

	clean := err == nil && ffCtx.Err() == nil
	s.readyMu.Lock()
	s.exited = true
	if clean {
		// The pass wrote every segment (`-segment_times` yields exactly
		// segmentCount files). Advance readyMax to the full count HERE, before
		// unblocking waiters: otherwise a handler woken by the readyCh close could
		// race ahead of the 250ms pollSegments tick that seals the LAST segment
		// and wrongly see "exited before segment ready".
		s.readyMax = s.segmentCount
	} else if err != nil && ffCtx.Err() == nil {
		s.exitErr = fmt.Errorf("copy-vod pass: %w (%s)", err, strings.TrimSpace(errBuf.String()))
	}
	if s.readyCh != nil {
		close(s.readyCh)
		s.readyCh = nil
	}
	s.readyMu.Unlock()

	if clean {
		log.Printf("[hls %s] copy-vod pass complete", shortHLSID(s.cfg.SessionID))
	}
}
