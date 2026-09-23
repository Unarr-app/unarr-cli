package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// A single-connection Close returns only once its ffmpeg is reaped — the
// provider socket goes with the process — and only then releases the source
// gate the next session is waiting on.
func TestSingleConnectionCloseWaitsForFFmpegReap(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("ffmpeg not on PATH: %v", err)
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skipf("ffprobe not on PATH: %v", err)
	}
	src := filepath.Join(t.TempDir(), "src.mkv")
	if out, err := exec.Command(ffmpeg, "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=60:size=640x360:rate=25",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", src).CombinedOutput(); err != nil {
		t.Fatalf("generate: %v\n%s", err, out)
	}

	// A slow provider keeps ffmpeg mid-read (not finished) when Close lands.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, err := os.Open(src)
		if err != nil {
			return
		}
		defer f.Close()
		fi, _ := f.Stat()
		w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		buf := make([]byte, 8<<10)
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
			if rerr != nil {
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}))
	defer ts.Close()

	var liveAtRelease = -1
	var s *HLSSession
	s, err = StartHLSSession(context.Background(), HLSSessionConfig{
		SessionID:        "reapwait",
		SourceURL:        ts.URL + "/src.mkv",
		SingleConnection: true,
		Quality:          "360p",
		AcquireSource: func(context.Context) (func(), error) {
			return func() {
				s.readyMu.Lock()
				liveAtRelease = s.liveProcs
				s.readyMu.Unlock()
			}, nil
		},
		Transcode: TranscodeRuntime{FFmpegPath: ffmpeg, FFprobePath: ffprobe, Preset: "ultrafast"},
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // ffmpeg running
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if liveAtRelease != 0 {
		t.Fatalf("source released with %d ffmpeg process(es) still alive", liveAtRelease)
	}
	if cmd.ProcessState == nil {
		t.Fatal("Close returned before ffmpeg was reaped")
	}
}
