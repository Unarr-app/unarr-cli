package engine

import (
	"context"
	"log"
	"math"
	"os"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

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
	if cfg.SingleConnection && os.Getenv(copyVODDirectEnv) == "1" {
		// A one-connection provider is only COPY-VOD'd through the source
		// proxy's single upstream link (index, IDR probe, segment spawns and
		// subtitle windows all share it). With the proxy switched off every
		// reader would open its own connection: use the linear EVENT copy.
		return false
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

// indexKeyframesFast obtains a complete exact table from the container metadata
// or a previously verified sidecar. A window/uniform approximation MUST NOT be
// published as VOD: the immutable playlist would lie about segment boundaries.
func indexKeyframesFast(ctx context.Context, s *HLSSession, src string) (starts []float64, exact bool, ok bool) {
	started := time.Now()
	var kfs []float64
	if s.cfg.SourceURL == "" {
		kfs, _ = mediainfo.ReadCachedKeyframes(src)
	}
	if len(kfs) == 0 {
		var err error
		kfs, err = copySourceKeyframes(ctx, s, src)
		if err != nil {
			return nil, false, false
		}
	}
	starts = validatedCopySegments(kfs, s.durationSec)
	if starts == nil {
		return nil, false, false
	}
	log.Printf("[hls %s] copy-vod exact index: %d points in %s", shortHLSID(s.cfg.SessionID), len(kfs), time.Since(started).Round(time.Millisecond))
	return starts, true, true
}

func copySourceKeyframes(ctx context.Context, s *HLSSession, src string) ([]float64, error) {
	kfs, err := mediainfo.ReadContainerKeyframes(ctx, src)
	if err == nil {
		return kfs, nil
	}
	// Never download a remote film just to build its playlist. Unsupported
	// local containers may still be small enough for a bounded demux.
	if s.cfg.SourceURL != "" {
		log.Printf("[hls %s] copy-vod exact remote index unavailable", shortHLSID(s.cfg.SessionID))
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	return mediainfo.IndexKeyframes(probeCtx, s.cfg.Transcode.FFprobePath, src)
}

// Reject stale/corrupt sidecars and overly sparse indexes instead of silently
// making huge fragments or manufacturing a first keyframe at t=0.
func validatedCopySegments(kfs []float64, duration float64) []float64 {
	if len(kfs) == 0 || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 {
		return nil
	}
	if kfs[0] < 0 || kfs[0] > 0.25 {
		return nil
	}
	previous := -1.0
	for _, kf := range kfs {
		if math.IsNaN(kf) || math.IsInf(kf, 0) || kf <= previous || kf >= duration {
			return nil
		}
		previous = kf
	}
	starts := planCopySegments(kfs, duration)
	for i := 1; i < len(starts); i++ {
		if starts[i]-starts[i-1] > 30 {
			return nil
		}
	}
	return starts
}
