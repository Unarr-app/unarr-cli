// Package engine — hls_copy_vod_segments.go: MPEG-TS segment generation for the
// COPY-VOD model (the model itself lives in hls_copy_vod.go). Two producers:
//
//   - PASS (local, preferred): buildCopyVODPassArgs — ONE `-f segment` ffmpeg
//     reads the file linearly (no seek) and cuts at the exact keyframe boundaries.
//     Correct on any container/fps because it never seeks; ensureCopySegment waits
//     on readyMax like encode mode.
//   - LAZY (remote/uniform, or local without disk headroom): buildCopyVODSegmentArgs
//     — one `-ss start -to end -c:v copy` spawn per requested segment, single-
//     flighted per index, written to a .tmp then atomically renamed. On VBR/scene-
//     cut sources the input `-ss` lands on the keyframe BEFORE the boundary (→ GOP
//     overlap / echo); the PASS exists precisely to avoid that for local files.
package engine

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// copyVODAudioArgs resolves the audio map + codec for a COPY-VOD segment,
// mirroring buildHLSCopyArgs exactly: copy AAC ≤2ch, re-encode everything else
// to AAC stereo 48k (so the TS always carries browser-safe AAC).
func copyVODAudioArgs(cfg HLSSessionConfig, probe *StreamProbe) []string {
	audioIdx := cfg.AudioIndex
	if audioIdx < 0 {
		audioIdx = 0
		for i, a := range probe.AudioTracks {
			if a.Default {
				audioIdx = i
				break
			}
		}
	}
	if n := len(probe.AudioTracks); n > 0 && audioIdx >= n {
		audioIdx = 0
	}
	args := []string{"-map", fmt.Sprintf("0:a:%d?", audioIdx)}
	audioCodec := probe.AudioCodec
	audioChannels := 0
	if audioIdx < len(probe.AudioTracks) {
		audioCodec = probe.AudioTracks[audioIdx].Codec
		audioChannels = probe.AudioTracks[audioIdx].Channels
	}
	if strings.EqualFold(audioCodec, "aac") && audioChannels > 0 && audioChannels <= 2 {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "192k", "-ar", "48000", "-ac", "2")
	}
	return args
}

// buildCopyVODSegmentArgs builds the ffmpeg invocation that produces ONE
// COPY-VOD MPEG-TS fragment for [start,end) into outPath. Correctness rests on:
//
//   - `-copyts`: keep the source's absolute timestamps so the segment's PTS
//     equals its real position. MPEG-TS carries them directly, so the player
//     places the segment correctly relative to its neighbours (no init needed).
//   - `-ss start` BEFORE `-i`: keyframe-accurate input seek. start is a real
//     keyframe time, so ffmpeg lands exactly on it (no preceding-GOP slop).
//   - `-to end`: an output limit against the copyts timestamps — stop at the
//     next boundary keyframe.
//
// Verified empirically (Wistoria S02E09, H.264+AAC): seg-N PTS = [start..end)
// with no gaps/overlap; hls.js shows the full duration + seeks anywhere.
func buildCopyVODSegmentArgs(cfg HLSSessionConfig, probe *StreamProbe, outPath string, start, end float64) []string {
	args := []string{
		"-y", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-copyts",
		"-ss", strconv.FormatFloat(start, 'f', 6, 64),
		"-i", cfg.sourceRef(),
		"-to", strconv.FormatFloat(end, 'f', 6, 64),
		"-map", "0:v:0",
	}
	args = append(args, copyVODAudioArgs(cfg, probe)...)
	// -bsf:v h264_mp4toannexb: H.264 in MP4/MKV is stored length-prefixed (avcC)
	// with SPS/PPS only in the container header. MPEG-TS needs in-band Annex-B
	// with SPS/PPS repeated per segment, else the segment is undecodable (mp4
	// sources produced 0-frame TS without this). No-op passthrough on a stream
	// already Annex-B, so it is applied unconditionally.
	args = append(args, "-c:v", "copy", "-bsf:v", "h264_mp4toannexb")
	args = append(args,
		"-muxdelay", "0", "-muxpreload", "0",
		"-f", "mpegts",
		outPath,
	)
	return args
}

// buildCopyVODPassArgs builds the SINGLE ffmpeg invocation that produces EVERY
// COPY-VOD segment in one linear read (LOCAL sources). It replaces the per-index
// `-ss` spawns, whose input-seek lands on the keyframe BEFORE the boundary on
// VBR/scene-cut sources → each segment re-emits ~1 GOP → the player echoes.
//
// The segment muxer instead reads sequentially (no seek) and cuts at the exact
// keyframe boundaries in `starts`, so segments are contiguous with zero overlap
// (measured: total frames == source, 0 dup, across mkv/mp4 @ 23.976–29.97fps).
//   - `-segment_times`: the interior boundaries (starts[1 : len-1]); starts[0]==0
//     and the final element (duration) are implicit.
//   - `mpegts_copyts=1`: keep absolute source PTS (no +1.4s TS base offset), so
//     the manifest EXTINF positions line up with each segment's real PTS.
//   - `-bsf:v h264_mp4toannexb`: same in-band SPS/PPS requirement as above.
//   - `-reset_timestamps 0`: absolute PTS across segments (seek-anywhere).
func buildCopyVODPassArgs(cfg HLSSessionConfig, probe *StreamProbe, starts []float64, tmpDir string) []string {
	args := []string{
		"-y", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", cfg.sourceRef(),
		"-map", "0:v:0",
	}
	args = append(args, copyVODAudioArgs(cfg, probe)...)
	args = append(args, "-c:v", "copy", "-bsf:v", "h264_mp4toannexb",
		"-muxpreload", "0", "-muxdelay", "0",
		"-f", "segment",
		"-reset_timestamps", "0",
		"-segment_format", "mpegts",
		"-segment_format_options", "mpegts_copyts=1",
	)
	// Interior boundaries only. With a single segment (starts=[0,dur]) there are
	// none — omit -segment_times so the muxer emits one segment for the whole file.
	if len(starts) > 2 {
		times := make([]string, 0, len(starts)-2)
		for i := 1; i < len(starts)-1; i++ {
			times = append(times, strconv.FormatFloat(starts[i], 'f', 6, 64))
		}
		args = append(args, "-segment_times", strings.Join(times, ","))
	}
	args = append(args, filepath.Join(tmpDir, "video", "seg-%d"+copyVODSegExt))
	return args
}

// ensureCopySegment makes seg-idx.ts available before it is served.
//
// PASS mode (local, copyLazy=false): a single background segment-muxer pass owns
// generation. Wait on readyMax — advanced by pollSegments only once the SUCCESSOR
// file exists, proving seg-idx is fully closed. No per-index stat fast-path here:
// the segment muxer writes each file in place (no .tmp+rename), so a Size>0 stat
// could read the segment the muxer is writing RIGHT NOW.
//
// LAZY mode (remote/uniform, or local without disk headroom): generate on demand
// via a per-index `-ss` spawn, single-flighted so concurrent fetches don't spawn
// duplicate ffmpegs.
func (s *HLSSession) ensureCopySegment(ctx context.Context, idx int) error {
	if !s.copyLazy {
		return s.waitForSegment(ctx, idx)
	}
	path := s.copySegPath(idx)
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
		return nil
	}
	gate := s.copyGenGate(idx)
	gate.Lock()
	defer gate.Unlock()
	// Re-check under the gate: a racer may have produced it while we waited.
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
		return nil
	}
	return s.generateCopySegment(ctx, idx)
}

// copySegPath is the on-disk path of COPY-VOD segment idx.
func (s *HLSSession) copySegPath(idx int) string {
	return filepath.Join(s.tmpDir, "video", fmt.Sprintf("seg-%d%s", idx, copyVODSegExt))
}

// copyGenGate returns the per-index mutex used to single-flight segment
// generation. A tiny map under copyGenMu — no external singleflight dep.
func (s *HLSSession) copyGenGate(idx int) *sync.Mutex {
	s.copyGenMu.Lock()
	defer s.copyGenMu.Unlock()
	if s.copyGen == nil {
		s.copyGen = make(map[int]*sync.Mutex)
	}
	g := s.copyGen[idx]
	if g == nil {
		g = &sync.Mutex{}
		s.copyGen[idx] = g
	}
	return g
}

// generateCopySegment runs ffmpeg to produce seg-idx.ts (written to a .tmp then
// atomically renamed, so a concurrent reader never sees a half-written file).
// Caller holds the per-index gate. Bounds: idx in [0, segmentCount).
func (s *HLSSession) generateCopySegment(ctx context.Context, idx int) error {
	if idx < 0 || idx >= s.segmentCount {
		return fmt.Errorf("hls: copy-vod segment %d out of range [0,%d)", idx, s.segmentCount)
	}
	start := s.copySegStarts[idx]
	end := s.copySegStarts[idx+1]
	final := s.copySegPath(idx)
	tmp := final + ".tmp"
	defer os.Remove(tmp) //nolint:errcheck — best-effort cleanup of a stale temp

	args := buildCopyVODSegmentArgs(s.cfg, s.probe, tmp, start, end)
	genCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(genCtx, s.cfg.Transcode.FFmpegPath, args...)
	winproc.HideWindow(cmd)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	startedAt := time.Now()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("hls: copy-vod seg-%d ffmpeg: %w (%s)", idx, err, strings.TrimSpace(errBuf.String()))
	}
	if fi, err := os.Stat(tmp); err != nil || fi.Size() == 0 {
		return fmt.Errorf("hls: copy-vod seg-%d not produced", idx)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("hls: copy-vod seg-%d publish: %w", idx, err)
	}
	log.Printf("[hls %s] copy-vod seg-%d ready (%.1f–%.1fs, %dms)",
		shortHLSID(s.cfg.SessionID), idx, start, end, time.Since(startedAt).Milliseconds())
	return nil
}
