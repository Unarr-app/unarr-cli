//go:build smoke

package engine

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestHLSCacheSmoke exercises the end-to-end cache flow against real ffmpeg:
//   - First session encodes a 5s test pattern; expect MISS, ffmpeg runs,
//     .complete written, MarkComplete logs.
//   - Second session for identical (source, quality, audio); expect HIT,
//     no ffmpeg, instant Start.
//
// Build tag `smoke` keeps it out of the default `go test ./...` run because
// it depends on a working ffmpeg/ffprobe and takes ~5–10 s.
//
//	go test -tags=smoke -run TestHLSCacheSmoke -v ./internal/engine/
func TestHLSCacheSmoke(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("ffmpeg not on PATH: %v", err)
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skipf("ffprobe not on PATH: %v", err)
	}

	tmp := t.TempDir()
	source := filepath.Join(tmp, "source.mp4")
	t.Logf("generating 5 s test pattern → %s", source)
	if out, err := exec.Command(ffmpeg,
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=5:size=640x480:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=5",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac",
		source,
	).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg generate: %v\n%s", err, out)
	}

	cacheRoot := filepath.Join(tmp, "cache")
	cache, err := NewHLSCache(cacheRoot, 1)
	if err != nil {
		t.Fatalf("NewHLSCache: %v", err)
	}

	cfg := HLSSessionConfig{
		SessionID:  "smoke1",
		SourcePath: source,
		FileName:   "source.mp4",
		Quality:    "720p",
		AudioIndex: 0,
		Transcode: TranscodeRuntime{
			FFmpegPath:  ffmpeg,
			FFprobePath: ffprobe,
			Preset:      "ultrafast",
		},
		Cache: cache,
	}

	// First run — expect MISS, ffmpeg runs.
	t.Log("session 1: expect MISS")
	t0 := time.Now()
	s1, err := StartHLSSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartHLSSession #1: %v", err)
	}
	if s1.fromCache {
		t.Fatal("session 1 reported cache HIT on a fresh cache")
	}

	// Wait for all segments to land. 5 s source @ 4 s segments → 2 segments.
	deadline := time.Now().Add(60 * time.Second)
	for {
		s1.readyMu.Lock()
		ready := s1.readyMax
		exited := s1.exited
		s1.readyMu.Unlock()
		if ready >= s1.segmentCount-1 && exited {
			break
		}
		if time.Now().After(deadline) {
			_ = s1.Close()
			t.Fatalf("session 1 didn't finish in 60 s (readyMax=%d/%d, exited=%v)",
				ready, s1.segmentCount-1, exited)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close #1: %v", err)
	}
	encodeDur := time.Since(t0)
	t.Logf("session 1: MISS completed in %s", encodeDur.Round(time.Millisecond))

	key := cache.KeyFor(source, "720p", 0)
	if !cache.HasComplete(key) {
		t.Fatalf("cache.HasComplete(%s) is false after successful encode", key)
	}

	// Second run — expect HIT, no ffmpeg.
	t.Log("session 2: expect HIT")
	cfg.SessionID = "smoke2"
	t1 := time.Now()
	s2, err := StartHLSSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartHLSSession #2: %v", err)
	}
	if !s2.fromCache {
		t.Fatal("session 2 should have reported cache HIT")
	}
	if s2.cmd != nil {
		t.Fatal("session 2 should not have spawned ffmpeg (s.cmd != nil)")
	}
	hitDur := time.Since(t1)
	t.Logf("session 2: HIT in %s (%.1f×  faster than MISS)",
		hitDur.Round(time.Millisecond), float64(encodeDur)/float64(hitDur))
	if hitDur > 500*time.Millisecond {
		t.Errorf("HIT path too slow: %s — expected <500 ms", hitDur)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("Close #2: %v", err)
	}

	// After the HIT session closes, the cache dir + .complete must still exist.
	if !cache.HasComplete(key) {
		t.Fatal(".complete disappeared after HIT session closed")
	}
}
