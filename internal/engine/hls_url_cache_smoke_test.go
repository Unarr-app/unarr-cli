//go:build smoke

package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestURLSessionsNeverReplayAnotherTitle reproduces B1 against real ffmpeg: two
// different provider titles (same duration, same quality) played in a row
// through URL sessions. The second must never be served from the first one's
// sealed cache — with no identity (no cache at all) nor with per-URL identities.
//
//	go test -tags=smoke -run TestURLSessionsNeverReplayAnotherTitle -v ./internal/engine/
func TestURLSessionsNeverReplayAnotherTitle(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("ffmpeg not on PATH: %v", err)
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skipf("ffprobe not on PATH: %v", err)
	}
	tmp := t.TempDir()
	for _, name := range []string{"e01.mkv", "e02.mkv"} {
		src := "testsrc"
		if name == "e02.mkv" {
			src = "smptebars"
		}
		if out, err := exec.Command(ffmpeg, "-y", "-loglevel", "error",
			"-f", "lavfi", "-i", src+"=duration=5:size=640x360:rate=25",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=5",
			"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac",
			filepath.Join(tmp, name)).CombinedOutput(); err != nil {
			t.Fatalf("generate %s: %v\n%s", name, err, out)
		}
	}
	ts := httptest.NewServer(http.FileServer(http.Dir(tmp)))
	defer ts.Close()

	run := func(t *testing.T, cache *HLSCache, id, url, cacheID string) *HLSSession {
		t.Helper()
		s, err := StartHLSSession(context.Background(), HLSSessionConfig{
			SessionID: id, SourceURL: url, CacheID: cacheID, Quality: "480p",
			Transcode: TranscodeRuntime{FFmpegPath: ffmpeg, FFprobePath: ffprobe, Preset: "ultrafast"},
			Cache:     cache, SingleConnection: true,
		})
		if err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
		deadline := time.Now().Add(60 * time.Second)
		for {
			s.readyMu.Lock()
			done := s.exited && s.readyMax >= s.segmentCount-1
			s.readyMu.Unlock()
			if done || s.fromCache {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s did not finish", id)
			}
			time.Sleep(100 * time.Millisecond)
		}
		return s
	}

	for _, tc := range []struct {
		name     string
		id1, id2 string
	}{
		{"no identity", "", ""},
		{"per-url identity", "url:e01", "url:e02"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache, err := NewHLSCache(filepath.Join(t.TempDir(), "cache"), 1)
			if err != nil {
				t.Fatal(err)
			}
			s1 := run(t, cache, "b1a", ts.URL+"/e01.mkv", tc.id1)
			_ = s1.Close()
			s2 := run(t, cache, "b1b", ts.URL+"/e02.mkv", tc.id2)
			defer s2.Close()
			if s2.fromCache {
				t.Fatalf("E02 was served from E01's cache (key %s)", s2.cacheKey)
			}
			// Same title again: with an identity it must now HIT (the cache still works).
			if tc.id1 != "" {
				s3 := run(t, cache, "b1c", ts.URL+"/e01.mkv", tc.id1)
				defer s3.Close()
				if !s3.fromCache {
					t.Fatal("replaying E01 with its own identity should hit the cache")
				}
			}
		})
	}
}
