package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

// Opt-in against the user's actual debrid CDN, never a local file or loopback
// substitute. Keep signed URLs out of logs and artifacts. PATH selects the same
// FFmpeg build used by the agent (it must support HTTPS).
func TestCopyVODRemoteRegression(t *testing.T) {
	src := os.Getenv("UNARR_BROWSER_REMOTE_URL")
	if src == "" {
		t.Skip("set UNARR_BROWSER_REMOTE_URL to a real debrid media URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	probe, err := ProbeFile(ctx, "ffprobe", src)
	if err != nil {
		t.Fatal(strings.ReplaceAll(err.Error(), src, "[remote source]"))
	}
	kfs, err := mediainfo.ReadContainerKeyframes(ctx, src)
	if err != nil {
		t.Fatal(strings.ReplaceAll(err.Error(), src, "[remote source]"))
	}
	starts := planCopySegments(kfs, probe.DurationSec)
	t.Logf("real remote media: %s, %.3fs, %d keyframes, %d exact segments", probe.VideoCodec, probe.DurationSec, len(kfs), len(starts)-1)
	cfg := HLSSessionConfig{SourceURL: src, VideoCopy: true, AudioIndex: -1}
	dir := t.TempDir()
	run := func(args []string) {
		t.Helper()
		out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ffmpeg: %v %s", err, strings.ReplaceAll(string(out), src, "[remote source]"))
		}
	}
	for _, legacy := range []bool{true, false} {
		lastVideo, lastAudio := math.NaN(), math.NaN()
		first := 0
		for i, p := range starts[:len(starts)-1] {
			if p >= 60 {
				first = i
				break
			}
		}
		for n := 0; n < 3 && first+n+1 < len(starts); n++ {
			start, end := starts[first+n], starts[first+n+1]
			path := filepath.Join(dir, fmt.Sprintf("%t-%d.ts", legacy, n))
			if legacy {
				start, end = float64(60+n*6), float64(66+n*6)
				// Frozen v1.14.0 remote/uniform implementation for comparison.
				args := []string{"-y", "-nostdin", "-v", "error", "-copyts", "-ss", fmt.Sprintf("%.6f", start), "-i", src, "-to", fmt.Sprintf("%.6f", end), "-map", "0:v:0"}
				args = append(args, copyVODAudioArgs(cfg, probe)...)
				args = append(args, "-c:v", "copy", "-bsf:v", "h264_mp4toannexb", "-muxdelay", "0", "-muxpreload", "0", "-f", "mpegts", path)
				run(args)
			} else {
				run(buildCopyVODSegmentArgs(cfg, probe, path, start, end))
			}
			minVideo, maxVideo, minAudio := math.Inf(1), math.Inf(-1), math.Inf(1)
			audioEnd := math.NaN()
			packetJSON, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_packets", "-show_entries", "packet=stream_index,pts_time,duration_time", "-of", "json", path).Output()
			if err != nil {
				t.Fatal(err)
			}
			var packets struct {
				Packets []continuityPacket `json:"packets"`
			}
			if err := json.Unmarshal(packetJSON, &packets); err != nil {
				t.Fatal(err)
			}
			for _, p := range packets.Packets {
				pts := continuityTime(p.PTS)
				if p.Stream == 0 {
					minVideo, maxVideo = math.Min(minVideo, pts), math.Max(maxVideo, pts)
				} else if p.Stream == 1 {
					minAudio = math.Min(minAudio, pts)
					audioEnd = pts + continuityTime(p.Duration)
				}
			}
			t.Logf("legacy=%t manifest=%.3f..%.3f video=%.6f..%.6f overlap=%.6f audioJoin=%+.6f", legacy, start, end, minVideo, maxVideo, lastVideo-minVideo, minAudio-lastAudio)
			if !legacy {
				if math.Abs(minVideo-start) > .002 || maxVideo >= end+.002 || minVideo <= lastVideo {
					t.Fatal("video cut mismatches exact manifest or replays prior frames")
				}
				if !math.IsNaN(lastAudio) && math.Abs(minAudio-lastAudio) > .0011 {
					t.Fatal("audio discontinuity at remote fragment boundary")
				}
				if out := continuityRun(t, "ffmpeg", "-v", "error", "-i", path, "-map", "0:v", "-f", "null", "-"); len(out) != 0 {
					t.Fatalf("fragment not independently decodable: %s", out)
				}
			}
			lastVideo, lastAudio = maxVideo, audioEnd
		}
	}
}
