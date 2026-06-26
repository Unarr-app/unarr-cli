// Package engine — hls_copy_vod_segments.go: on-demand MPEG-TS segment
// generation for the COPY-VOD model (the model itself lives in hls_copy_vod.go).
//
// One ffmpeg `-ss start -i src -to end -c:v copy -f mpegts` spawn produces one
// keyframe-bounded fragment when the player requests it. Generation is
// single-flighted per segment index so concurrent fetches (or a fetch racing a
// prewarm) never spawn duplicate ffmpegs, and each segment is written to a .tmp
// then atomically renamed so a reader never sees a half-written file.
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
	args = append(args, "-c:v", "copy")
	args = append(args,
		"-muxdelay", "0", "-muxpreload", "0",
		"-f", "mpegts",
		outPath,
	)
	return args
}

// ensureCopySegment generates seg-idx.ts on demand if it is not already on
// disk, single-flighting concurrent requests for the SAME index so two player
// fetches (or a fetch racing a prewarm) never spawn duplicate ffmpegs.
func (s *HLSSession) ensureCopySegment(ctx context.Context, idx int) error {
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
