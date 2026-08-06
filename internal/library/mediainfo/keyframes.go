package mediainfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Keyframe index sidecar. The COPY-VOD streaming model (engine) needs the sorted
// keyframe timestamps of a local file to plan its segment boundaries. Indexing is
// a full demux-only ffprobe pass (reads the whole container), so caching the tiny
// result next to the media — same `.unarr` sidecar scheme as subtitles/thumbnails
// — lets the scan-time prewarm pay that read ONCE and every later play start
// instantly instead of re-demuxing the file. Invalidated by mtime like the others.

// CopyVODEligibleCodec reports whether a video codec can ride COPY-VOD's MPEG-TS
// transport (H.264 only): TS carries it universally; HEVC needs fMP4 (Apple HLS)
// and AV1 isn't a TS codec. Placed in this leaf package so the stream engine
// (segment planning) and the scan-time prewarm (which items to keyframe-index)
// classify codecs identically — same rationale as IsTextSubtitleCodec.
func CopyVODEligibleCodec(videoCodec string) bool {
	switch strings.ToLower(strings.TrimSpace(videoCodec)) {
	case "h264", "avc", "avc1":
		return true
	}
	return false
}

// Keyframe-index timeout budget. The old flat 45 s was sized against a WARM
// cache (a 12 GB h264 re-indexes in ~4 s once the pages are resident) but was
// applied to the COLD path too. Measured over NFS v3, cold: a 12 GB h264 needs
// 153 s and a 15 GB one 252 s — so EVERY cold index died at exactly 45.0 s with
// "signal: killed ()" and dropped the session to EVENT copy, which cannot honour
// a resume position at all.
//
// Size is only a rough proxy: the real cost driver is keyframe DENSITY, which
// size cannot see. Measured cold, per GB: 12.7 s (Ballerina, 15 GB, 1100 kf) but
// >30 s (Marty Supreme, 6 GB, a telesync whose GOPs are so dense the window
// alone yielded 774 keyframes in 300 s) — a 2.5× spread between two files of the
// same codec. The budget below is therefore deliberately loose, and the real
// protection against a slow index is IndexKeyframeWindow (the caller starts from
// a bounded window and finishes the full index in the background) rather than
// any per-GB number tuned here.
const (
	copyKeyframeIndexMinTimeout = 5 * time.Minute
	copyKeyframeIndexMaxTimeout = 20 * time.Minute
	copyKeyframeIndexPerGB      = 40 * time.Second

	// A window is a bounded read and sits on the latency-critical path — its job
	// is to answer fast or get out of the way, so it is capped far below the
	// full-file budget. Measured over NFS: 7 s for a 300 s window on an idle
	// mount, but 43 s for a 600 s window while the same mount was busy — so the
	// cap has to clear the busy case, or the one path that saves a cold resume
	// dies exactly when the box is under load and needs it most.
	copyKeyframeWindowTimeout = 2 * time.Minute
)

// ErrKeyframeIndexTimeout marks an index aborted by its own deadline rather than
// by a real demux failure. Callers distinguish the two because they mean opposite
// things: a timeout says "too slow here, try again later / prewarm it", while a
// demux error says "this file will never index". Mirrors ErrProbeExpired.
var ErrKeyframeIndexTimeout = errors.New("keyframe index timed out")

// keyframeIndexTimeout sizes the budget from the file's size, falling back to the
// floor when it can't be stat'ed (a remote/unstattable source indexes fast or not
// at all).
func keyframeIndexTimeout(mediaPath string) time.Duration {
	fi, err := os.Stat(mediaPath)
	if err != nil {
		return copyKeyframeIndexMinTimeout
	}
	const gib = 1024 * 1024 * 1024
	budget := time.Duration(fi.Size()/gib) * copyKeyframeIndexPerGB
	if budget < copyKeyframeIndexMinTimeout {
		return copyKeyframeIndexMinTimeout
	}
	if budget > copyKeyframeIndexMaxTimeout {
		return copyKeyframeIndexMaxTimeout
	}
	return budget
}

// keyframeSidecarPath is the cached keyframe-index JSON path for mediaPath.
func keyframeSidecarPath(mediaPath string) string {
	return filepath.Join(sidecarDir(mediaPath), fmt.Sprintf("%s.copyseg.json", filepath.Base(mediaPath)))
}

type keyframeSidecar struct {
	Keyframes []float64 `json:"keyframes"`
}

// IndexKeyframes returns the sorted presentation timestamps (seconds) of every
// video keyframe in mediaPath.
//
// It reads PACKET headers (`-show_entries packet=pts_time,flags`) and keeps the
// ones flagged keyframe ("K") — a demux-only pass, NOT a decode. This is ~40×
// faster than `-skip_frame nokey` (which decodes each keyframe). Still a full
// demux of the container, hence local-file only. Errors if no keyframes are found.
func IndexKeyframes(ctx context.Context, ffprobePath, mediaPath string) ([]float64, error) {
	return indexKeyframes(ctx, ffprobePath, mediaPath, "")
}

// IndexKeyframeWindow indexes ONLY the keyframes in [fromSec, fromSec+windowSec)
// via ffprobe's `-read_intervals`, so a cold start can plan segments around the
// user's resume point without paying the full-file demux (measured over NFS: 7 s
// for a 300 s window vs 252 s for the whole 15 GB file). The timestamps it
// returns are identical to the corresponding slice of a full index — verified
// against one — so a window is a true subset, never an approximation.
//
// fromSec <= 0 windows from the start of the file.
func IndexKeyframeWindow(ctx context.Context, ffprobePath, mediaPath string, fromSec, windowSec float64) ([]float64, error) {
	if windowSec <= 0 {
		return nil, errors.New("keyframe window: non-positive window")
	}
	// ffprobe interval syntax: "START%+DURATION"; omitting START reads from the
	// beginning. Seconds with no unit suffix are what ffprobe expects here.
	interval := fmt.Sprintf("%%+%s", strconv.FormatFloat(windowSec, 'f', 3, 64))
	if fromSec > 0 {
		interval = strconv.FormatFloat(fromSec, 'f', 3, 64) + interval
	}
	return indexKeyframes(ctx, ffprobePath, mediaPath, interval)
}

// indexKeyframes is the shared demux+parse used by the full and windowed index.
// readInterval empty = whole file.
func indexKeyframes(ctx context.Context, ffprobePath, mediaPath, readInterval string) ([]float64, error) {
	// Only impose the default ceiling when the caller set no deadline. The
	// playback path (no deadline) gets the size-derived cap so a stuck index can't
	// strand the player; the scan-time prewarm passes its own longer budget
	// (a huge cold remux may need minutes) and that must win, not be clamped.
	budget := time.Duration(0) // 0 = caller's own deadline, reported as such below
	if _, ok := ctx.Deadline(); !ok {
		budget = indexBudget(mediaPath, readInterval)
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	out, err := runKeyframeProbe(ctx, ffprobePath, mediaPath, readInterval, budget)
	if err != nil {
		return nil, err
	}
	kfs, err := parseKeyframeCSV(out)
	if err != nil {
		return nil, err
	}
	if len(kfs) == 0 {
		return nil, errors.New("keyframe index: no keyframes found")
	}
	sort.Float64s(kfs)
	return kfs, nil
}

// ReadCachedKeyframes returns the cached keyframe index when a fresh sidecar
// exists. ok=false means the caller should IndexKeyframes on demand.
func ReadCachedKeyframes(mediaPath string) ([]float64, bool) {
	p := keyframeSidecarPath(mediaPath)
	if !sidecarFresh(p, mediaPath) {
		return nil, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	var sc keyframeSidecar
	if err := json.Unmarshal(b, &sc); err != nil || len(sc.Keyframes) == 0 {
		return nil, false
	}
	return sc.Keyframes, true
}

// WriteCachedKeyframes stores the keyframe index next to the media. Best-effort
// (a read-only mount just means no cache; the on-demand index still works).
func WriteCachedKeyframes(mediaPath string, kfs []float64) error {
	if len(kfs) == 0 {
		return errors.New("refusing to cache empty keyframe index")
	}
	b, err := json.Marshal(keyframeSidecar{Keyframes: kfs})
	if err != nil {
		return err
	}
	return writeSidecar(keyframeSidecarPath(mediaPath), b)
}

// PrewarmKeyframes indexes + caches the keyframe table for mediaPath unless a
// fresh sidecar already exists. Best-effort, idempotent — the scan-time prewarm
// job. Returns nil (no work) when the cache is already fresh.
func PrewarmKeyframes(ctx context.Context, ffprobePath, mediaPath string) error {
	if sidecarFresh(keyframeSidecarPath(mediaPath), mediaPath) {
		return nil
	}
	kfs, err := IndexKeyframes(ctx, ffprobePath, mediaPath)
	if err != nil {
		return err
	}
	return WriteCachedKeyframes(mediaPath, kfs)
}
