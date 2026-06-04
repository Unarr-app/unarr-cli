package mediainfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ErrTrickplayInProgress means another worker — possibly an agent on another host
// sharing the same library (e.g. the dev binary on /mnt/nas and the docker agent
// on /downloads, the SAME files) — already holds this sprite's lock and is
// generating it. The caller must SKIP, not count it as a failure.
var ErrTrickplayInProgress = errors.New("trickplay: generation already in progress")

// trickplayLockTTL bounds a stale lock: longer than the caller's 45-min generation
// deadline so a live job is never stolen, short enough that a crashed/killed
// worker's lock is reclaimed on a later scan.
const trickplayLockTTL = 90 * time.Minute

// acquireTrickplayLock takes an exclusive, cross-process lock for one sprite by
// O_CREATE|O_EXCL on a ".lock" file in the shared sidecar dir, so two agents that
// watch the same library never decode the same 4K file at once (the cause of the
// 5×-per-file ffmpeg pile-up). A lock older than trickplayLockTTL is assumed
// abandoned (owner crashed) and reclaimed. Returns ErrTrickplayInProgress when a
// fresh lock is held by someone else.
func acquireTrickplayLock(lockPath string) (func(), error) {
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			host, _ := os.Hostname()
			fmt.Fprintf(f, "%s pid=%d t=%d\n", host, os.Getpid(), time.Now().Unix())
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("trickplay lock: %w", err)
		}
		if fi, statErr := os.Stat(lockPath); statErr == nil && time.Since(fi.ModTime()) > trickplayLockTTL {
			_ = os.Remove(lockPath) // stale → reclaim and retry
			continue
		}
		return nil, ErrTrickplayInProgress
	}
	return nil, ErrTrickplayInProgress
}

// TrickplayManifest describes the montage sprite layout so a client can map a
// playback time to one tile: tileIndex = floor(timeSec / IntervalSec), then
// col = tileIndex % Cols, row = tileIndex / Cols, and the tile's pixel box is
// (col*TileWidth, row*TileHeight, TileWidth, TileHeight).
type TrickplayManifest struct {
	Version     int     `json:"version"` // schema version (1)
	IntervalSec float64 `json:"intervalSec"`
	TileWidth   int     `json:"tileWidth"`
	TileHeight  int     `json:"tileHeight"`
	Cols        int     `json:"cols"`
	Rows        int     `json:"rows"`
	Count       int     `json:"count"` // number of REAL frames (≤ Cols*Rows; the rest are padding)
	DurationSec float64 `json:"durationSec"`
}

// trickplaySpritePath / trickplayManifestPath include the tile width so changing
// library.trickplay.width regenerates cleanly instead of serving a stale sprite.
func trickplaySpritePath(mediaPath string, width int) string {
	return filepath.Join(sidecarDir(mediaPath), fmt.Sprintf("%s.trickplay.w%d.jpg", filepath.Base(mediaPath), width))
}

func trickplayManifestPath(mediaPath string, width int) string {
	return filepath.Join(sidecarDir(mediaPath), fmt.Sprintf("%s.trickplay.w%d.json", filepath.Base(mediaPath), width))
}

// TrickplaySpritePath is the public accessor the stream server uses to locate the
// cached sprite JPEG for serving.
func TrickplaySpritePath(mediaPath string, width int) string {
	return trickplaySpritePath(mediaPath, width)
}

// ReadCachedTrickplay returns the manifest when a fresh sprite + manifest exist
// for (mediaPath, width). ok=false means the caller should (re)generate. Both
// the sprite and the manifest must be at least as new as the media file.
func ReadCachedTrickplay(mediaPath string, width int) (TrickplayManifest, bool) {
	sprite := trickplaySpritePath(mediaPath, width)
	manifest := trickplayManifestPath(mediaPath, width)
	if !sidecarFresh(sprite, mediaPath) || !sidecarFresh(manifest, mediaPath) {
		return TrickplayManifest{}, false
	}
	b, err := os.ReadFile(manifest)
	if err != nil || len(b) == 0 {
		return TrickplayManifest{}, false
	}
	var m TrickplayManifest
	if err := json.Unmarshal(b, &m); err != nil || m.Cols <= 0 || m.TileWidth <= 0 {
		return TrickplayManifest{}, false
	}
	return m, true
}

// GenerateTrickplay builds the montage sprite + manifest for mediaPath and caches
// them in the sidecar dir. ONE ffmpeg pass samples a frame every intervalSec
// (fps=1/interval), scales each to width (even height), and tiles them into a
// single JPEG. The whole file is decoded once — slow but a one-time, cached,
// scan-time cost (run with idle I/O priority by the prewarm), and it removes ALL
// live extraction during playback (no contention with the active stream).
//
// durationSec drives the grid size; pass the probed duration (0 → error, nothing
// to sample). The caller owns the ctx deadline (generous at scan time).
func GenerateTrickplay(ctx context.Context, ffmpegPath, mediaPath string, intervalSec float64, width int, durationSec float64) (TrickplayManifest, error) {
	if ffmpegPath == "" {
		return TrickplayManifest{}, fmt.Errorf("trickplay: no ffmpeg")
	}
	if intervalSec <= 0 || width <= 0 {
		return TrickplayManifest{}, fmt.Errorf("trickplay: invalid interval=%v width=%d", intervalSec, width)
	}
	if durationSec <= 0 {
		return TrickplayManifest{}, fmt.Errorf("trickplay: unknown duration")
	}

	// fps=1/interval emits a frame at t=0, interval, 2*interval, … while t <
	// duration → ceil(duration/interval) frames. (An earlier floor(...)+1 put a
	// black padding tile at the very end of the scrubber on round-duration media.)
	effInterval := intervalSec
	count := int(math.Ceil(durationSec / effInterval))
	if count < 1 {
		count = 1
	}

	// Mobile decode cap: a single JPEG above ~16.7M px (4096²) fails to decode on
	// iOS/Safari. For long media, sample fewer frames (coarser effective interval)
	// so ONE sprite stays renderable everywhere. tileH is unknown until probe, so
	// estimate from 16:9 for the budget; the manifest reports effInterval so the
	// client maps time→tile correctly.
	const maxSpritePixels = 16_000_000
	estTileH := width * 9 / 16
	if estTileH < 1 {
		estTileH = 1
	}
	if maxTiles := maxSpritePixels / (width * estTileH); maxTiles >= 1 && count > maxTiles {
		effInterval = durationSec / float64(maxTiles)
		count = int(math.Ceil(durationSec / effInterval))
		if count > maxTiles {
			count = maxTiles // guard ceil rounding
		}
	}

	// Roughly-square grid. Cols*Rows ≥ count; trailing cells are ffmpeg padding,
	// and Count tells the client how many are real.
	cols := int(math.Ceil(math.Sqrt(float64(count))))
	if cols < 1 {
		cols = 1
	}
	rows := int(math.Ceil(float64(count) / float64(cols)))
	if rows < 1 {
		rows = 1
	}

	spritePath := trickplaySpritePath(mediaPath, width)
	manifestPath := trickplayManifestPath(mediaPath, width)
	if err := os.MkdirAll(filepath.Dir(spritePath), 0o755); err != nil {
		return TrickplayManifest{}, err
	}

	// Single-flight across processes/agents: only one worker decodes this file at
	// a time. Returns ErrTrickplayInProgress (skip, not fail) if another holds it.
	release, err := acquireTrickplayLock(spritePath + ".lock")
	if err != nil {
		return TrickplayManifest{}, err
	}
	defer release()

	tmpSprite := spritePath + ".tmp"

	// fps filter wants a rational; format 1/effInterval with enough precision.
	fps := fmt.Sprintf("1/%s", strconv.FormatFloat(effInterval, 'f', 3, 64))
	vf := fmt.Sprintf("fps=%s,scale=%d:-2,tile=%dx%d", fps, width, cols, rows)
	args := []string{
		"-nostdin", "-loglevel", "error", "-y",
		"-i", mediaPath,
		"-frames:v", "1",
		"-vf", vf,
		"-an", "-sn",
		"-q:v", "5",
		// Force the muxer: the temp output ends in ".tmp", so ffmpeg can't infer
		// the format from the extension (it would error "Unable to choose an
		// output format"). mjpeg writes the single montage frame as a JPEG.
		"-f", "mjpeg",
		tmpSprite,
	}
	// Pin this goroutine to its OS thread for the whole child lifetime. hardenCmd's
	// Pdeathsig is delivered when the THREAD that forked dies, not the process
	// (golang/go#27505); without the lock Go could recycle that thread mid-decode
	// and the kernel would SIGKILL a perfectly healthy ffmpeg. Locked here (before
	// the fork in Start) and released after Wait, the thread lives exactly as long
	// as ffmpeg: it dies only when the agent process itself dies → SIGKILL fires
	// only then, which is precisely the orphan we want to prevent.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	// Die-with-parent BEFORE Start so an agent crash can't orphan this decode.
	hardenCmd(cmd)
	// Start + idle I/O + lowest CPU niceness + Wait (matches the subtitle/thumbnail
	// extractors): this full-decode pass is the heaviest sidecar job and runs in the
	// background alongside live streaming on the same box/NFS, so it must yield both
	// disk AND CPU. The prewarm also gates it on system load before getting here.
	if err := cmd.Start(); err != nil {
		_ = os.Remove(tmpSprite)
		return TrickplayManifest{}, fmt.Errorf("ffmpeg tile start: %w", err)
	}
	setIdleIOPriority(cmd.Process.Pid)
	setLowCPUPriority(cmd.Process.Pid)
	if err := cmd.Wait(); err != nil {
		_ = os.Remove(tmpSprite)
		return TrickplayManifest{}, fmt.Errorf("ffmpeg tile: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if fi, err := os.Stat(tmpSprite); err != nil || fi.Size() == 0 {
		_ = os.Remove(tmpSprite)
		return TrickplayManifest{}, fmt.Errorf("trickplay: empty sprite")
	}

	// Probe the produced sprite for EXACT dimensions, so tile geometry is precise
	// (avoids ±1px aspect-rounding drift between our math and ffmpeg's scale=-2).
	spriteW, spriteH, err := probeImageDims(ctx, ffmpegPath, tmpSprite)
	if err != nil || spriteW < cols || spriteH < rows {
		_ = os.Remove(tmpSprite)
		return TrickplayManifest{}, fmt.Errorf("trickplay: probe sprite dims: %w", err)
	}
	m := TrickplayManifest{
		Version:     1,
		IntervalSec: effInterval,
		TileWidth:   spriteW / cols,
		TileHeight:  spriteH / rows,
		Cols:        cols,
		Rows:        rows,
		Count:       count,
		DurationSec: durationSec,
	}
	mb, err := json.Marshal(m)
	if err != nil {
		_ = os.Remove(tmpSprite)
		return TrickplayManifest{}, err
	}
	// Publish sprite (rename) then manifest (atomic write). Order: sprite first so
	// a reader that sees a fresh manifest always finds the matching sprite.
	if err := os.Rename(tmpSprite, spritePath); err != nil {
		_ = os.Remove(tmpSprite)
		return TrickplayManifest{}, err
	}
	if err := writeSidecar(manifestPath, mb); err != nil {
		return TrickplayManifest{}, err
	}
	return m, nil
}

// probeImageDims returns the pixel width/height of an image file via ffmpeg's
// bundled ffprobe-less path: we reuse ffmpeg with -hide_banner and parse the
// "Stream ... WxH" line from stderr. Using ffmpeg (already resolved) avoids a
// hard dependency on a separate ffprobe binary here.
func probeImageDims(ctx context.Context, ffmpegPath, path string) (int, int, error) {
	cmd := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-i", path)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	_ = cmd.Run() // ffmpeg exits non-zero with no output file; we only want the probe stderr
	return parseDims(stderr.String())
}

// parseDims extracts the first WxH (e.g. "3840x2160") from ffmpeg's stream info.
func parseDims(s string) (int, int, error) {
	idx := strings.Index(s, "Video:")
	if idx < 0 {
		return 0, 0, fmt.Errorf("no video stream in probe output")
	}
	// Scan for the first "<digits>x<digits>" token after "Video:".
	rest := s[idx:]
	for i := 0; i < len(rest); i++ {
		if rest[i] < '0' || rest[i] > '9' {
			continue
		}
		j := i
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		if j < len(rest) && rest[j] == 'x' {
			k := j + 1
			for k < len(rest) && rest[k] >= '0' && rest[k] <= '9' {
				k++
			}
			if k > j+1 {
				w, _ := strconv.Atoi(rest[i:j])
				h, _ := strconv.Atoi(rest[j+1 : k])
				if w > 0 && h > 0 {
					return w, h, nil
				}
			}
		}
		i = j
	}
	return 0, 0, fmt.Errorf("no WxH token in probe output")
}
