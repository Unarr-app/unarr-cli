//go:build smoke

package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSingleConnectionSessionOpensOneProviderConnection plays an h264 mkv with a
// text subtitle track as a single-connection copy session — COPY-VOD through
// the source proxy's single upstream link (index, IDR probe, segment spawns,
// prefetch, subtitle windows) — seeks far ahead and back, and checks the
// "provider" never sees two connections at once. Also exercises a resume
// (StartSec), which COPY-VOD honours without falling back to a transcode.
//
//	go test -tags=smoke -run TestSingleConnectionSessionOpensOneProviderConnection -v ./internal/engine/
func TestSingleConnectionSessionOpensOneProviderConnection(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("ffmpeg not on PATH: %v", err)
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skipf("ffprobe not on PATH: %v", err)
	}
	tmp := t.TempDir()
	srt := filepath.Join(tmp, "s.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:03,000\nhola\n\n2\n00:00:20,000 --> 00:00:22,000\nadios\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(tmp, "movie.mkv")
	if out, err := exec.Command(ffmpeg, "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=120:size=640x360:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=120",
		"-i", srt,
		"-map", "0:v", "-map", "1:a", "-map", "2:s",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-c:s", "srt", src).CombinedOutput(); err != nil {
		t.Fatalf("generate: %v\n%s", err, out)
	}

	for _, tc := range []struct {
		name     string
		startSec float64
	}{
		{"copy from the start", 0},
		{"resume deep in the file", 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counter := &countingHandler{next: http.FileServer(http.Dir(tmp))}
			ts := httptest.NewServer(counter)
			defer ts.Close()
			s, err := StartHLSSession(context.Background(), HLSSessionConfig{
				SessionID:        "one" + filepath.Base(tc.name)[:3],
				SourceURL:        ts.URL + "/movie.mkv",
				VideoCopy:        true,
				CopyVideoCodecs:  []string{"h264"},
				SingleConnection: true,
				StartSec:         tc.startSec,
				Quality:          "480p",
				Transcode:        TranscodeRuntime{FFmpegPath: ffmpeg, FFprobePath: ffprobe, Preset: "ultrafast"},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			// Seekable COPY-VOD over the proxy's single upstream link: a full
			// VOD manifest from the first fetch, a resume honoured in copy.
			if !s.copyVOD || s.copyProxy == nil {
				t.Fatal("single-connection h264 session did not take COPY-VOD through the source proxy")
			}
			if !strings.Contains(s.manifestVideo, "#EXT-X-ENDLIST") {
				t.Fatal("manifest is not a complete VOD playlist (no seek past the copied window)")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			// Play the start, then seek far ahead and back: each segment is served
			// while the prefetcher and subtitle windows share the same link.
			for _, idx := range []int{0, 1, s.segmentCount - 2, s.segmentCount / 2, 2} {
				if err := s.ensureCopySegment(ctx, idx); err != nil {
					t.Fatalf("seg-%d: %v", idx, err)
				}
			}
			time.Sleep(time.Second) // prefetch + subtitle windows keep reading
			if m := counter.maxSeen.Load(); m != 1 {
				t.Fatalf("provider saw %d concurrent connections, want 1", m)
			}
		})
	}
}
