package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

type continuityPacket struct {
	Stream   int    `json:"stream_index"`
	PTS      string `json:"pts_time"`
	DTS      string `json:"dts_time"`
	Duration string `json:"duration_time"`
	Flags    string `json:"flags"`
}

func continuityRun(t *testing.T, bin string, args ...string) []byte {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", bin, err, out)
	}
	return out
}

func continuityPackets(t *testing.T, path string) []continuityPacket {
	t.Helper()
	out := continuityRun(t, "ffprobe", "-v", "error", "-show_packets", "-show_entries", "packet=stream_index,pts_time,dts_time,duration_time,flags", "-of", "json", path)
	var data struct {
		Packets []continuityPacket `json:"packets"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		t.Fatal(err)
	}
	return data.Packets
}

func continuityTime(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }

func continuityHashes(t *testing.T, path string) []string {
	t.Helper()
	out := continuityRun(t, "ffmpeg", "-v", "error", "-i", path, "-map", "0:v:0", "-an", "-fps_mode", "passthrough", "-f", "framemd5", "-")
	var hashes []string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 6 {
			t.Fatalf("decode error or invalid frame: %s", line)
		}
		hashes = append(hashes, strings.TrimSpace(fields[5]))
	}
	return hashes
}

// A real FFmpeg regression, not an assertion about argument strings. Compare
// EVERY decoded picture with the source, and every audio packet's timestamp
// with its predecessor. Old input-seek-only cuts replay frames and fail this.
func TestCopyVODContinuity(t *testing.T) {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " unavailable")
		}
	}
	for _, audio := range []string{"aac", "ac3"} {
		for _, ext := range []string{"mp4", "mkv"} {
			t.Run(audio+"/"+ext, func(t *testing.T) {
				dir := t.TempDir()
				src := filepath.Join(dir, "source."+ext)
				continuityRun(t, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=192x108:rate=24000/1001", "-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000", "-t", "30", "-c:v", "libx264", "-g", "96", "-bf", "3", "-sc_threshold", "0", "-c:a", audio, "-ac", "2", src)
				probe, err := ProbeFile(context.Background(), "ffprobe", src)
				if err != nil {
					t.Fatal(err)
				}
				kfs, err := mediainfo.ReadContainerKeyframes(context.Background(), src)
				if err != nil {
					t.Fatal(err)
				}
				starts := planCopySegments(kfs, probe.DurationSec)
				var hashes []string
				lastAudioEnd := math.NaN()
				for i := 0; i < len(starts)-1; i++ {
					path := filepath.Join(dir, fmt.Sprintf("seg-%d.ts", i))
					args := buildCopyVODSegmentArgs(HLSSessionConfig{SourcePath: src, AudioIndex: -1}, probe, path, starts[i], starts[i+1])
					continuityRun(t, "ffmpeg", args...)
					hashes = append(hashes, continuityHashes(t, path)...)
					firstVideo, firstAudio := true, true
					for _, p := range continuityPackets(t, path) {
						pts := continuityTime(p.PTS)
						if p.Stream == 0 && firstVideo {
							if !strings.Contains(p.Flags, "K") {
								t.Fatal("segment not independently decodable")
							}
							if math.Abs(pts-starts[i]) > 0.05 {
								t.Fatalf("segment %d starts %.6f, manifest %.6f", i, pts, starts[i])
							}
							firstVideo = false
						}
						if p.Stream != 1 {
							continue
						}
						if firstAudio && (pts < starts[i]-0.0001 || pts-starts[i] > 0.022) {
							t.Fatalf("audio %d starts %.6f, cut %.6f", i, pts, starts[i])
						}
						firstAudio = false
						if !math.IsNaN(lastAudioEnd) && math.Abs(pts-lastAudioEnd) > 0.0011 {
							t.Fatalf("audio discontinuity in segment %d: %.6f → %.6f (%+.6f)", i, lastAudioEnd, pts, pts-lastAudioEnd)
						}
						lastAudioEnd = pts + continuityTime(p.Duration)
					}
					if firstVideo || firstAudio {
						t.Fatal("missing media stream")
					}
				}
				want := continuityHashes(t, src)
				if len(hashes) != len(want) {
					t.Fatalf("frames %d want %d (duplicates or missing frames)", len(hashes), len(want))
				}
				for i := range want {
					if hashes[i] != want[i] {
						t.Fatalf("frame %d differs from source", i)
					}
				}
				t.Logf("%d decoded frames identical; audio contiguous across %d segments", len(want), len(starts)-1)
			})
		}
	}
}

func TestCopyVODRealMediaCuts(t *testing.T) {
	src := os.Getenv("UNARR_CONTINUITY_MEDIA")
	if src == "" {
		t.Skip("set UNARR_CONTINUITY_MEDIA")
	}
	probe, err := ProbeFile(context.Background(), "ffprobe", src)
	if err != nil {
		t.Fatal(err)
	}
	kfs, err := mediainfo.ReadContainerKeyframes(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	starts := planCopySegments(kfs, probe.DurationSec)
	dir := t.TempDir()
	for _, center := range []int{0, len(starts) / 2, len(starts) - 4} {
		lastAudioEnd := math.NaN()
		lastVideoPTS := math.NaN()
		for i := center; i < center+3 && i < len(starts)-1; i++ {
			path := filepath.Join(dir, fmt.Sprintf("seg-%d.ts", i))
			continuityRun(t, "ffmpeg", buildCopyVODSegmentArgs(HLSSessionConfig{SourcePath: src, AudioIndex: -1}, probe, path, starts[i], starts[i+1])...)
			firstVideo := true
			for _, p := range continuityPackets(t, path) {
				pts := continuityTime(p.PTS)
				if p.Stream == 0 {
					if firstVideo && math.Abs(pts-starts[i]) > 0.05 {
						t.Fatalf("seg-%d PTS %.6f expected %.6f", i, pts, starts[i])
					}
					if !math.IsNaN(lastVideoPTS) && firstVideo && pts <= lastVideoPTS {
						t.Fatalf("replayed video in seg-%d", i)
					}
					lastVideoPTS = math.Max(pts, func() float64 {
						if math.IsNaN(lastVideoPTS) {
							return pts
						}
						return lastVideoPTS
					}())
					firstVideo = false
				} else if p.Stream == 1 {
					if !math.IsNaN(lastAudioEnd) && math.Abs(pts-lastAudioEnd) > 0.0011 {
						t.Fatalf("audio discontinuity seg-%d: %.6f → %.6f", i, lastAudioEnd, pts)
					}
					lastAudioEnd = pts + continuityTime(p.Duration)
				}
			}
			// Decode independently; FFmpeg error logging must remain empty.
			out := continuityRun(t, "ffmpeg", "-v", "error", "-i", path, "-map", "0:v", "-f", "null", "-")
			if len(out) != 0 {
				t.Fatalf("segment decode errors: %s", out)
			}
			t.Logf("seg-%d %.3f..%.3f valid", i, starts[i], starts[i+1])
		}
	}
}
