//go:build smoke

package engine

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// HLS-copy integration suite — real ffmpeg, synthetic sources replicating
// every shape that broke the progressive-remux path in production:
//
//	h264+aac mkv     → video copy + audio copy
//	h264+ac3 mkv     → video copy + audio re-encode (the priming-dts class
//	                   that needed delay_moov on the old remux)
//	hevc10+eac3 mkv  → the exact "Hoppers" incident shape (Main10, hvc1 tag)
//	resume (-ss)     → StartSec mid-file, timeline offset
//
// Asserts on every run: ffmpeg's playlist reaches ENDLIST, EXTINF sum ≈
// source duration, every listed segment exists non-empty, ffprobe decodes
// the served playlist with the EXPECTED codecs, and the video stream was
// NOT re-encoded (copy must preserve the source codec).
//
//	go test -tags=smoke -run TestHLSCopy -v ./internal/engine/
func copyTestRuntime(t *testing.T) TranscodeRuntime {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("ffmpeg not on PATH: %v", err)
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skipf("ffprobe not on PATH: %v", err)
	}
	return TranscodeRuntime{FFmpegPath: ffmpeg, FFprobePath: ffprobe}
}

// genSource synthesises a test file. encV/encA are the SOURCE encoders; skip
// the test when the local ffmpeg lacks them (libx265 is optional in some
// builds).
func genSource(t *testing.T, rt TranscodeRuntime, name string, vArgs, aArgs []string, durSec int) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	args := []string{
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=duration=%d:size=640x360:rate=30", durSec),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", durSec),
	}
	args = append(args, vArgs...)
	args = append(args, aArgs...)
	// Short GOP so the copy cuts several segments even on a short source.
	args = append(args, "-g", "60", "-keyint_min", "60", out)
	if outB, err := exec.Command(rt.FFmpegPath, args...).CombinedOutput(); err != nil {
		if strings.Contains(string(outB), "Unknown encoder") {
			t.Skipf("source encoder unavailable: %s", string(outB))
		}
		t.Fatalf("generate %s: %v\n%s", name, err, outB)
	}
	return out
}

// runCopySession starts a VideoCopy session and waits for ffmpeg's playlist
// to reach ENDLIST. Returns the session and the final playlist text.
func runCopySession(t *testing.T, rt TranscodeRuntime, source string, startSec float64) (*HLSSession, string) {
	t.Helper()
	s, err := StartHLSSession(context.Background(), HLSSessionConfig{
		SessionID:  "copytest" + strconv.FormatInt(time.Now().UnixNano()%1_000_000, 10),
		SourcePath: source,
		FileName:   filepath.Base(source),
		AudioIndex: -1,
		StartSec:   startSec,
		VideoCopy:  true,
		Transcode:  rt,
	})
	if err != nil {
		t.Fatalf("StartHLSSession(copy): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	playlistPath := filepath.Join(s.tmpDir, "video", copyPlaylistName)
	deadline := time.Now().Add(30 * time.Second)
	for {
		data, err := os.ReadFile(playlistPath)
		if err == nil && strings.Contains(string(data), "#EXT-X-ENDLIST") {
			return s, string(data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("playlist never reached ENDLIST; last read err=%v contents:\n%s", err, string(data))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// assertCopyOutput validates playlist structure, segment files, and (via
// ffprobe over the playlist) that the served stream carries the expected
// codecs — wantVideo MUST equal the source codec, proving no re-encode.
func assertCopyOutput(t *testing.T, rt TranscodeRuntime, s *HLSSession, playlist, wantVideo, wantAudio string, wantDur float64) {
	t.Helper()
	if !strings.Contains(playlist, "#EXT-X-PLAYLIST-TYPE:EVENT") {
		t.Errorf("playlist missing EVENT type:\n%s", playlist)
	}
	if !strings.Contains(playlist, `#EXT-X-MAP:URI="init.mp4"`) {
		t.Errorf("playlist missing EXT-X-MAP init.mp4")
	}

	var sum float64
	segs := 0
	for _, line := range strings.Split(playlist, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXTINF:") {
			v := strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ",")
			d, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("bad EXTINF %q: %v", line, err)
			}
			sum += d
		} else if strings.HasSuffix(line, ".m4s") {
			segs++
			fi, err := os.Stat(filepath.Join(s.tmpDir, "video", line))
			if err != nil || fi.Size() == 0 {
				t.Errorf("listed segment %s missing/empty: %v", line, err)
			}
		}
	}
	if segs == 0 {
		t.Fatalf("no segments listed:\n%s", playlist)
	}
	if sum < wantDur-1.5 || sum > wantDur+1.5 {
		t.Errorf("EXTINF sum = %.2fs, want ≈%.2fs (±1.5)", sum, wantDur)
	}

	// ffprobe over the playlist = a real demuxer consuming init + segments.
	out, err := exec.Command(rt.FFprobePath, "-v", "error",
		"-show_entries", "stream=codec_type,codec_name",
		"-of", "csv=p=0",
		filepath.Join(s.tmpDir, "video", copyPlaylistName)).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe playlist: %v\n%s", err, out)
	}
	probeStr := string(out)
	if !strings.Contains(probeStr, wantVideo+",video") && !strings.Contains(probeStr, "video,"+wantVideo) &&
		!strings.Contains(probeStr, wantVideo) {
		t.Errorf("video codec: probe=%q want %q (copy must NOT re-encode)", probeStr, wantVideo)
	}
	if !strings.Contains(probeStr, wantAudio) {
		t.Errorf("audio codec: probe=%q want %q", probeStr, wantAudio)
	}
}

func TestHLSCopy_H264AacCopyBoth(t *testing.T) {
	rt := copyTestRuntime(t)
	src := genSource(t, rt, "h264aac.mkv",
		[]string{"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p"},
		[]string{"-c:a", "aac", "-b:a", "128k"}, 8)
	s, pl := runCopySession(t, rt, src, 0)
	assertCopyOutput(t, rt, s, pl, "h264", "aac", 8)
	// Audio already AAC → the args must COPY it, not re-encode.
	args := buildHLSCopyArgs(s.cfg, s.probe, s.tmpDir)
	if !containsSeq(args, "-c:a", "copy") {
		t.Errorf("expected -c:a copy for AAC source, args: %v", args)
	}
}

func TestHLSCopy_H264Ac3TranscodesAudio(t *testing.T) {
	rt := copyTestRuntime(t)
	src := genSource(t, rt, "h264ac3.mkv",
		[]string{"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p"},
		[]string{"-c:a", "ac3", "-b:a", "192k"}, 8)
	s, pl := runCopySession(t, rt, src, 0)
	// The re-encoded AAC track starts with a priming dts — the exact shape
	// that produced a malformed init on the old progressive remux. The HLS
	// muxer must land a probe-clean stream regardless.
	assertCopyOutput(t, rt, s, pl, "h264", "aac", 8)
	args := buildHLSCopyArgs(s.cfg, s.probe, s.tmpDir)
	if !containsSeq(args, "-c:a", "aac") {
		t.Errorf("expected -c:a aac for AC3 source, args: %v", args)
	}
}

func TestHLSCopy_Hevc10Eac3_IncidentShape(t *testing.T) {
	rt := copyTestRuntime(t)
	src := genSource(t, rt, "hevc10eac3.mkv",
		[]string{"-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le", "-x265-params", "log-level=error"},
		[]string{"-c:a", "eac3", "-b:a", "192k"}, 8)
	s, pl := runCopySession(t, rt, src, 0)
	assertCopyOutput(t, rt, s, pl, "hevc", "aac", 8)
	// HEVC must carry the hvc1 tag or Safari refuses the track.
	args := buildHLSCopyArgs(s.cfg, s.probe, s.tmpDir)
	if !containsSeq(args, "-tag:v", "hvc1") {
		t.Errorf("expected -tag:v hvc1 for HEVC source, args: %v", args)
	}
}

func TestHLSCopy_ResumeStartSec(t *testing.T) {
	rt := copyTestRuntime(t)
	src := genSource(t, rt, "resume.mkv",
		[]string{"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p"},
		[]string{"-c:a", "aac", "-b:a", "128k"}, 12)
	_, pl := runCopySession(t, rt, src, 6)
	// StartSec must be IGNORED in copy mode: the playlist covers the FULL
	// timeline from 0 (an offset EVENT playlist breaks iOS's native parser;
	// the player seeks to the resume point itself). Sum ≈ full 12s.
	var sum float64
	for _, line := range strings.Split(pl, "\n") {
		if strings.HasPrefix(line, "#EXTINF:") {
			v := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "#EXTINF:"), ",")
			d, _ := strconv.ParseFloat(v, 64)
			sum += d
		}
	}
	if sum < 10.5 || sum > 13.5 {
		t.Errorf("copy EXTINF sum = %.2fs, want ≈12s (StartSec ignored, full timeline)", sum)
	}
}

func TestHLSCopy_ServeVideoPlaylistFromDisk(t *testing.T) {
	rt := copyTestRuntime(t)
	src := genSource(t, rt, "serve.mkv",
		[]string{"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p"},
		[]string{"-c:a", "aac", "-b:a", "128k"}, 6)
	s, _ := runCopySession(t, rt, src, 0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/hls/x/video/index.m3u8", nil)
	s.ServeVideoPlaylist(rec, req)
	if rec.Code != 200 {
		t.Fatalf("ServeVideoPlaylist = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "#EXT-X-ENDLIST") || !strings.Contains(body, "seg-0.m4s") {
		t.Errorf("served playlist incomplete:\n%s", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Errorf("Content-Type = %q", ct)
	}

	// Master: no CODECS attr (a wrong hardcoded string makes iOS reject the
	// variant; omission is legal), real resolution present.
	master := s.MasterPlaylist()
	if strings.Contains(master, "CODECS") {
		t.Errorf("copy master must omit CODECS:\n%s", master)
	}
	if !strings.Contains(master, "RESOLUTION=640x360") {
		t.Errorf("copy master missing real resolution:\n%s", master)
	}
}

func containsSeq(args []string, a, b string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}
