package mediainfo

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestContainerIndexMatchesDemux(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	mp4 := filepath.Join(dir, "bframes.mp4")
	cmd := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24000/1001", "-t", "24", "-c:v", "libx264", "-g", "96", "-bf", "3", "-sc_threshold", "0", mp4)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	for _, ext := range []string{"mp4", "mkv"} {
		t.Run(ext, func(t *testing.T) {
			src := filepath.Join(dir, "bframes."+ext)
			if ext == "mkv" {
				if out, err := exec.Command(ffmpeg, "-v", "error", "-i", mp4, "-c", "copy", src).CombinedOutput(); err != nil {
					t.Fatalf("remux: %v %s", err, out)
				}
			}
			want, err := IndexKeyframes(context.Background(), ffprobe, src)
			if err != nil {
				t.Fatal(err)
			}
			check := func(src string) {
				t.Helper()
				got, err := ReadContainerKeyframes(context.Background(), src)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != len(want) {
					t.Fatalf("keyframes %v want %v", got, want)
				}
				for i := range got {
					if math.Abs(got[i]-want[i]) > 0.000002 {
						t.Fatalf("keyframe %d %.9f want %.9f", i, got[i], want[i])
					}
				}
			}
			check(src)
			var requests atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				requests.Add(1)
				if req.Header.Get("Range") == "" {
					t.Error("missing range")
				}
				http.ServeFile(w, req, src)
			}))
			defer srv.Close()
			check(srv.URL + "/source")
			if n := requests.Load(); n > 6 {
				t.Errorf("too many range reads: %d", n)
			}
		})
	}
}

func TestContainerIndexRejectsBadRanges(t *testing.T) {
	for _, kind := range []string{"ignored", "wrong-offset", "short", "huge", "compressed"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "ignored" {
					w.WriteHeader(200)
					return
				}
				v := "bytes 0-65535/100000"
				if kind == "wrong-offset" {
					v = "bytes 1-65536/100000"
				}
				if kind == "huge" {
					v = "bytes 0-999999/1000000"
				}
				if kind == "compressed" {
					w.Header().Set("Content-Encoding", "gzip")
				}
				w.Header().Set("Content-Range", v)
				w.WriteHeader(206)
				fmt.Fprint(w, "truncated")
			}))
			defer srv.Close()
			if _, err := ReadContainerKeyframes(context.Background(), srv.URL); err == nil {
				t.Fatal("accepted invalid range")
			}
		})
	}
}

// Opt-in acceptance proof: no mutation/sidecar writes to the user's library.
func TestContainerIndexRealMedia(t *testing.T) {
	src := os.Getenv("UNARR_INDEX_MEDIA")
	if src == "" {
		t.Skip("set UNARR_INDEX_MEDIA")
	}
	kfs, err := ReadContainerKeyframes(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	want, err := IndexKeyframes(context.Background(), "ffprobe", src)
	if err != nil {
		t.Fatal(err)
	}
	matched := 0
	for _, kf := range kfs {
		for _, w := range want {
			if math.Abs(kf-w) < 0.000002 {
				matched++
				break
			}
		}
	}
	if matched != len(kfs) {
		t.Fatalf("only %d/%d points are real keyframes", matched, len(kfs))
	}
	t.Logf("%d seek points verified against %d demux keyframes", matched, len(want))
}

func FuzzContainerSeekIndex(f *testing.F) {
	f.Add([]byte{0x1a, 0x45, 0xdf, 0xa3, 0x80, 0x18, 0x53, 0x80, 0x67, 0xff})
	f.Add([]byte{0, 0, 0, 8, 'm', 'o', 'o', 'v'})
	f.Add([]byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1024*1024 {
			t.Skip()
		}
		p := filepath.Join(t.TempDir(), "untrusted.media")
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
		_, _ = ReadContainerKeyframes(context.Background(), p)
	})
}
