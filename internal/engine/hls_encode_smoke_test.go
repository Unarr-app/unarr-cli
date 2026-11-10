//go:build smoke

package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// encodeSmokeWait is how long a session gets to land its first segment. A real
// encode of a few seconds of 1080p takes far less; only a dead encoder gets here.
const encodeSmokeWait = 90 * time.Second

// waitFirstSegment returns nil once the session's writer produced its first
// segment, or the reason it never will. It reads the same signals the daemon's
// ready-watcher uses, so a failure here is the failure a user sees.
func waitFirstSegment(s *HLSSession) error {
	deadline := time.Now().Add(encodeSmokeWait)
	for time.Now().Before(deadline) {
		if s.FromCache() || s.ReadyCount() > s.WriterStartIdx() {
			return nil
		}
		if failed, exitErr := s.Failed(); failed {
			return fmt.Errorf("encoder gave up: %v", exitErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("no segment after %s", encodeSmokeWait)
}

// TestRejectedSourceIsRecognised pins isSourceRejectedLine to what the real ffmpeg
// prints for a refused source, on the platform it runs on.
func TestRejectedSourceIsRecognised(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("ffmpeg not on PATH: %v", err)
	}
	for _, status := range []int{403, 410, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer ts.Close()
			out, err := exec.Command(ffmpeg, "-hide_banner", "-i", ts.URL+"/a.mkv", "-f", "null", "-").CombinedOutput()
			if err == nil {
				t.Fatalf("ffmpeg accepted a %d: %s", status, out)
			}
			recognised := false
			for _, line := range strings.Split(string(out), "\n") {
				recognised = recognised || isSourceRejectedLine(line)
			}
			if !recognised {
				t.Errorf("no line recognised for %d (%v):\n%s", status, err, out)
			}
			if !isHTTPExit(err) {
				t.Errorf("exit for %d not recognised as HTTP: %v", status, err)
			}
			t.Logf("%d -> %v", status, err)
		})
	}
}

// TestEncoderFailureCarriesStderr makes the real encoder give up and checks what
// the session hands the daemon for its report.
func TestEncoderFailureCarriesStderr(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("ffmpeg not on PATH: %v", err)
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skipf("ffprobe not on PATH: %v", err)
	}
	t.Run("bad preset", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "a.mkv")
		if out, err := exec.Command(ffmpeg, "-y", "-loglevel", "error",
			"-f", "lavfi", "-i", "testsrc=duration=5:size=640x360:rate=24",
			"-c:v", "libx264", "-preset", "ultrafast", src).CombinedOutput(); err != nil {
			t.Fatalf("generate: %v\n%s", err, out)
		}
		s, err := StartHLSSession(context.Background(), HLSSessionConfig{
			SessionID: "smkfail", SourcePath: src, Quality: "original",
			Transcode: TranscodeRuntime{FFmpegPath: ffmpeg, FFprobePath: ffprobe, Preset: "no-such-preset"},
		})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		defer func() { _ = s.Close() }()
		if err := waitFirstSegment(s); err == nil {
			t.Fatal("expected the encoder to give up")
		}
		tail := s.EncoderErrorTail()
		t.Logf("tail: %s", tail)
		if !strings.Contains(strings.ToLower(tail), "preset") {
			t.Errorf("tail does not name the cause: %q", tail)
		}
		if s.SourceRejected() {
			t.Error("a local encoder failure is not a rejected source")
		}
	})
}

// TestEncodeSessionProducesFirstSegment runs real encodes end to end through
// StartHLSSession: this is what cross-compiling and mocked ffmpeg cannot prove,
// and what test/windows/smoke-encode.ps1 drives inside the Windows VM.
//
//	go test -tags=smoke -run TestEncodeSessionProducesFirstSegment -v ./internal/engine/
func TestEncodeSessionProducesFirstSegment(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("ffmpeg not on PATH: %v", err)
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skipf("ffprobe not on PATH: %v", err)
	}

	tmp := t.TempDir()
	sources := []struct {
		name string
		args []string
	}{
		{"h264-aac.mkv", []string{"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac"}},
		{"hevc10-eac3.mkv", []string{"-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le", "-tag:v", "hvc1", "-c:a", "eac3"}},
	}
	for _, src := range sources {
		args := []string{
			"-y", "-loglevel", "error",
			"-f", "lavfi", "-i", "testsrc=duration=20:size=1920x1080:rate=24",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=20",
		}
		args = append(args, src.args...)
		args = append(args, filepath.Join(tmp, src.name))
		if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("generate %s: %v\n%s", src.name, err, out)
		}
	}
	ts := httptest.NewServer(http.FileServer(http.Dir(tmp)))
	defer ts.Close()

	for _, src := range sources {
		for _, quality := range []string{"1080p", "720p", "original"} {
			for _, remote := range []bool{false, true} {
				name := fmt.Sprintf("%s/%s/remote=%v", src.name, quality, remote)
				t.Run(name, func(t *testing.T) {
					cfg := HLSSessionConfig{
						SessionID: fmt.Sprintf("smk%d", time.Now().UnixNano()),
						Quality:   quality,
						Transcode: TranscodeRuntime{FFmpegPath: ffmpeg, FFprobePath: ffprobe, Preset: "ultrafast"},
					}
					if remote {
						cfg.SourceURL = ts.URL + "/" + src.name
						cfg.CacheID = "url:" + src.name + quality
					} else {
						cfg.SourcePath = filepath.Join(tmp, src.name)
					}
					cacheDir := filepath.Join(t.TempDir(), "hls-cache")
					cache, err := NewHLSCache(cacheDir, 1)
					if err != nil {
						t.Fatal(err)
					}
					cfg.Cache = cache
					s, err := StartHLSSession(context.Background(), cfg)
					if err != nil {
						t.Fatalf("start: %v", err)
					}
					defer func() { _ = s.Close() }()
					if err := waitFirstSegment(s); err != nil {
						entries, _ := os.ReadDir(filepath.Join(s.tmpDir, "video"))
						names := make([]string, 0, len(entries))
						for _, e := range entries {
							names = append(names, e.Name())
						}
						t.Fatalf("%v (video dir: %v)", err, names)
					}
					// A landed segment is not enough: the player fetches init.mp4
					// through ServeInit, which only looks in the session's own dir.
					init := httptest.NewRecorder()
					s.ServeInit(init, httptest.NewRequest(http.MethodGet, "/init.mp4", nil))
					if init.Code != http.StatusOK || init.Body.Len() == 0 {
						t.Errorf("ServeInit = %d with %d bytes", init.Code, init.Body.Len())
					}
					// What a player decodes: the served init + the first segment.
					seg, err := os.ReadFile(filepath.Join(s.tmpDir, "video", s.segmentFileName(0)))
					if err != nil {
						t.Fatalf("read first segment: %v", err)
					}
					joined := filepath.Join(t.TempDir(), "first.mp4")
					if err := os.WriteFile(joined, append(init.Body.Bytes(), seg...), 0o600); err != nil {
						t.Fatal(err)
					}
					out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0",
						"-show_entries", "stream=codec_name", "-of", "csv=p=0", joined).CombinedOutput()
					if err != nil || strings.TrimSpace(string(out)) != "h264" {
						t.Errorf("init + seg-0 is not a playable h264 stream: %v: %s", err, out)
					}
					if _, err := os.Stat("init.mp4"); err == nil {
						t.Error("ffmpeg wrote init.mp4 into the working directory")
					}
				})
			}
		}
	}
}
