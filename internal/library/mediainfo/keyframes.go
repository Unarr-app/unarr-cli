package mediainfo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// copyKeyframeIndexTimeout bounds the demux-only keyframe scan. A local 2 h film
// indexes in a few seconds off warm cache; past this ceiling the caller falls
// back rather than stranding the player.
const copyKeyframeIndexTimeout = 45 * time.Second

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
	ctx, cancel := context.WithTimeout(ctx, copyKeyframeIndexTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "packet=pts_time,flags",
		"-of", "csv=p=0",
		mediaPath,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("keyframe index: %w (%s)", err, strings.TrimSpace(errBuf.String()))
	}
	kfs := make([]float64, 0, 1024)
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// Each line is "pts_time,flags", e.g. "6.006000,K__". The flags field
		// carries "K" for a keyframe (RAP). Keep only those.
		line := strings.TrimSpace(sc.Text())
		comma := strings.IndexByte(line, ',')
		if comma < 0 {
			continue
		}
		ptsStr := strings.TrimSpace(line[:comma])
		flags := line[comma+1:]
		if !strings.Contains(flags, "K") {
			continue
		}
		if ptsStr == "" || ptsStr == "N/A" {
			continue
		}
		v, err := strconv.ParseFloat(ptsStr, 64)
		if err != nil || v < 0 {
			continue
		}
		kfs = append(kfs, v)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("keyframe index scan: %w", err)
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
