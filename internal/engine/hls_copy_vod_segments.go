// Package engine — exact, on-demand MPEG-TS segment generation.
package engine

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
// COPY-VOD MPEG-TS fragment for [start,end). Input seeking is only an I/O
// optimisation: with stream copy FFmpeg may return a PREVIOUS GOP even when
// start is a keyframe. The bitstream filter explicitly owns whole GOPs by their
// keyframe PTS. Filtering individual picture PTS would lose reordered B-frames.
// Audio owns packets on [start,end), independently of video decode order.
func buildCopyVODSegmentArgs(cfg HLSSessionConfig, probe *StreamProbe, outPath string, start, end float64) []string {
	// Read enough preroll for audio decoder/resampler/encoder warmup. Absolute
	// seek timestamps prevent FFmpeg adding the container start_time a second time.
	seek := math.Max(0, start-1)
	args := []string{
		"-y", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-copyts",
		"-seek_timestamp", "1",
		"-ss", strconv.FormatFloat(seek, 'f', 6, 64),
		"-i", cfg.sourceRef(),
		// Let reordered packets reach the filter; membership is NOT set by -to.
		"-to", strconv.FormatFloat(end+2, 'f', 6, 64),
		"-map", "0:v:0",
	}
	audio := copyVODAudioArgs(cfg, probe)
	args = append(args, audio...)
	if audio[len(audio)-1] != "copy" {
		// All AAC encoders start on the SAME 1024-sample lattice. Otherwise each
		// seek resets priming/rounding and adjacent segments overlap or drift.
		// Warm up before the cut; discard priming packets via bsf:a below.
		grid := int64(math.Floor(math.Max(0, start-0.25)*48000/1024)) * 1024
		args = append(args, "-af", fmt.Sprintf("aresample=48000:async=1:first_pts=%d", grid))
	}
	// -bsf:v h264_mp4toannexb: H.264 in MP4/MKV is stored length-prefixed (avcC)
	// with SPS/PPS only in the container header. MPEG-TS needs in-band Annex-B
	// with SPS/PPS repeated per segment, else the segment is undecodable (mp4
	// sources produced 0-frame TS without this). No-op passthrough on a stream
	// already Annex-B, so it is applied unconditionally.
	videoDrop := fmt.Sprintf("h264_mp4toannexb,noise=amount=0:drop='if(key,st(0,gte(pts*tb,%.9f)*lt(pts*tb,%.9f)));not(ld(0))'", start-0.000001, end-0.000001)
	// Older FFmpeg versions put buffering-period SEI before the repeated SPS.
	// Prepend the converted Annex-B extradata to each retained key packet so
	// a fresh decoder has its SPS/PPS before parsing those SEI messages too.
	videoDrop += ",dump_extra=freq=keyframe"
	audioDrop := fmt.Sprintf("noise=amount=0:drop='lt(pts*tb,%.9f)+gte(pts*tb,%.9f)'", start-0.000001, end-0.000001)
	args = append(args, "-c:v", "copy", "-bsf:v", videoDrop, "-bsf:a", audioDrop)
	args = append(args,
		"-avoid_negative_ts", "disabled", "-mpegts_copyts", "1",
		"-muxdelay", "0", "-muxpreload", "0",
		"-f", "mpegts",
		outPath,
	)
	return args
}

// copySegPath is the on-disk path of COPY-VOD segment idx.
func (s *HLSSession) copySegPath(idx int) string {
	return filepath.Join(s.tmpDir, "video", fmt.Sprintf("seg-%d%s", idx, copyVODSegExt))
}

// generateCopySegment runs ffmpeg to produce seg-idx.ts (written to a .tmp then
// atomically renamed, so a concurrent reader never sees a half-written file).
// Caller is the single-flight run for idx (hls_copy_vod_pipeline.go). Bounds:
// idx in [0, segmentCount).
func (s *HLSSession) generateCopySegment(ctx context.Context, idx int) error {
	if idx < 0 || idx >= s.segmentCount {
		return fmt.Errorf("hls: copy-vod segment %d out of range [0,%d)", idx, s.segmentCount)
	}
	start := s.copySegStarts[idx]
	end := s.copySegStarts[idx+1]
	final := s.copySegPath(idx)
	tmp := final + ".tmp"
	defer os.Remove(tmp) //nolint:errcheck // Best-effort cleanup of a stale temp.

	// A remote source is read through the session's range-caching proxy.
	cfg := s.cfg
	if cfg.SourceURL != "" {
		cfg.SourceURL = s.copySource()
	}
	args := buildCopyVODSegmentArgs(cfg, s.probe, tmp, start, end)
	genCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(genCtx, s.cfg.Transcode.FFmpegPath, args...)
	winproc.HideWindow(cmd)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	startedAt := time.Now()
	if err := cmd.Run(); err != nil {
		// A cancelled run (seek away, session close) surfaces from exec as
		// "signal: killed". Report the cancellation itself so callers can tell it
		// from a real ffmpeg failure; the 60 s genCtx timeout stays an error.
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return fmt.Errorf("hls: copy-vod seg-%d ffmpeg: %w (%s)", idx, err, strings.TrimSpace(errBuf.String()))
	}
	if fi, err := os.Stat(tmp); err != nil || fi.Size() == 0 {
		return fmt.Errorf("hls: copy-vod seg-%d not produced", idx)
	}
	if err := s.validateCopySegment(genCtx, tmp, start); err != nil {
		return err
	}
	if err := genCtx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("hls: copy-vod seg-%d publish: %w", idx, err)
	}
	log.Printf("[hls %s] copy-vod seg-%d ready (%.1f-%.1fs, %dms)",
		shortHLSID(s.cfg.SessionID), idx, start, end, time.Since(startedAt).Milliseconds())
	return nil
}

// A wrong/stale seek table must fail before publication, not silently splice
// the next GOP into a segment whose manifest promises a different start. Read
// just the first video packet; this does not decode video or scan the fragment.
func (s *HLSSession) validateCopySegment(ctx context.Context, path string, start float64) error {
	packet, length, err := firstCopyVideoPacket(ctx, s.cfg.Transcode.FFprobePath, path, "%+#1")
	if err != nil {
		return fmt.Errorf("hls: validate copy segment: %w", err)
	}
	if !strings.Contains(packet.Flags, "K") || !copyPacketHasIDR(packet.Data, length) {
		return fmt.Errorf("hls: copy segment has no independent IDR picture")
	}
	pts, err := strconv.ParseFloat(packet.PTS, 64)
	tolerance := 0.002
	if start == 0 {
		tolerance = 0.25
	} // normal encoder audio/video start offset
	if err != nil || math.IsNaN(pts) || math.IsInf(pts, 0) || math.Abs(pts-start) > tolerance {
		return fmt.Errorf("hls: copy segment starts at %s, expected %.6f", packet.PTS, start)
	}
	return nil
}
