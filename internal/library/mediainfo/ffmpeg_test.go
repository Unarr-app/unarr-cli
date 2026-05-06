package mediainfo

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestResolveFFmpeg_ExplicitOK verifies the explicit-path branch returns
// the requested binary if it exists on disk.
func TestResolveFFmpeg_ExplicitOK(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}

	got, err := ResolveFFmpeg(fake)
	if err != nil {
		t.Fatalf("ResolveFFmpeg(explicit): %v", err)
	}
	if got != fake {
		t.Fatalf("got %q want %q", got, fake)
	}
}

// TestResolveFFmpeg_ExplicitMissing returns a clear error when the path
// the operator supplied doesn't exist — we do NOT silently fall back.
func TestResolveFFmpeg_ExplicitMissing(t *testing.T) {
	_, err := ResolveFFmpeg("/nonexistent/path/ffmpeg-XXXXXX")
	if err == nil {
		t.Fatal("expected error for missing explicit path")
	}
}

// TestResolveFFmpeg_EnvVar honours FFMPEG_PATH when no explicit path is given.
func TestResolveFFmpeg_EnvVar(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	t.Setenv("FFMPEG_PATH", fake)
	// Hide the real ffmpeg from PATH so the env var is the next branch hit.
	t.Setenv("PATH", "/nonexistent")

	got, err := ResolveFFmpeg("")
	if err != nil {
		t.Fatalf("ResolveFFmpeg(env): %v", err)
	}
	if got != fake {
		t.Fatalf("got %q want %q (env-var branch)", got, fake)
	}
}

// TestFFmpegCachePath returns a sibling path to the ffprobe cache,
// consistent with the install layout the tarball produces.
func TestFFmpegCachePath(t *testing.T) {
	got, err := FFmpegCachePath()
	if err != nil {
		t.Fatalf("FFmpegCachePath: %v", err)
	}
	want := "ffmpeg"
	if runtime.GOOS == "windows" {
		want = "ffmpeg.exe"
	}
	if filepath.Base(got) != want {
		t.Fatalf("cache path basename = %q want %q", filepath.Base(got), want)
	}
	probeCache, err := FFprobeCachePath()
	if err != nil {
		t.Fatalf("FFprobeCachePath: %v", err)
	}
	if filepath.Dir(got) != filepath.Dir(probeCache) {
		t.Fatalf("ffmpeg cache (%s) and ffprobe cache (%s) should share a directory", got, probeCache)
	}
}
