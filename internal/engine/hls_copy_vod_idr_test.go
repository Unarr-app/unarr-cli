package engine

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyPacketHasIDR(t *testing.T) {
	for _, tc := range []struct {
		name, dump string
		size       int
		want       bool
	}{
		{"AVCC IDR", "\n00000000: 0000 0002 6580                           ....e.\n", 4, true},
		{"AVCC recovery I", "\n00000000: 0000 0002 4180                           ....A.\n", 4, false},
		{"AnnexB IDR", "\n00000000: 0000 0001 6580                           ....e.\n", 0, true},
		{"AnnexB recovery I", "\n00000000: 0000 0001 4180                           ....A.\n", 0, false},
		{"truncated", "\n00000000: 0000 0008 6580                           ....e.\n", 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := copyPacketHasIDR(tc.dump, tc.size); got != tc.want {
				t.Fatalf("got %t", got)
			}
		})
	}
}

func TestCopyVODOpenGOPUsesSeekableHLS(t *testing.T) {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " unavailable")
		}
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "open-gop.mp4")
	continuityRun(t, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=192x108:rate=30000/1001", "-t", "15",
		"-c:v", "libx264", "-g", "95", "-bf", "3", "-x264-params", "open-gop=1:scenecut=0", src)
	s, err := StartHLSSession(context.Background(), HLSSessionConfig{
		SessionID: fmt.Sprintf("open-gop-%s", filepath.Base(dir)), SourcePath: src, VideoCopy: true, AudioIndex: -1,
		Transcode: TranscodeRuntime{FFmpegPath: "ffmpeg", FFprobePath: "ffprobe", Preset: "ultrafast"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.copyVOD || s.cfg.VideoCopy || !s.copyNeedsEncode {
		t.Fatal("open GOP advertised as independent COPY-VOD")
	}
	if !strings.Contains(s.manifestVideo, "#EXT-X-ENDLIST") || !strings.Contains(s.manifestVideo, ".m4s") {
		t.Fatal("lost seekable HLS fallback")
	}
	if err := s.waitForSegment(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
}
