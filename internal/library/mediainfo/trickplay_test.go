package mediainfo

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestParseDims(t *testing.T) {
	cases := []struct {
		in   string
		w, h int
		ok   bool
	}{
		{"Stream #0:0: Video: mjpeg, yuvj420p(pc), 720x270 [SAR 1:1 DAR 8:3]", 720, 270, true},
		{"  Stream #0:0: Video: h264 (High), yuv420p, 3840x2160, 23.98 fps", 3840, 2160, true},
		{"Stream #0:1: Audio: aac, 48000 Hz, stereo", 0, 0, false}, // no Video:
		{"", 0, 0, false},
	}
	for _, c := range cases {
		w, h, err := parseDims(c.in)
		if c.ok {
			if err != nil || w != c.w || h != c.h {
				t.Errorf("parseDims(%q) = %d,%d,%v; want %d,%d,nil", c.in, w, h, err, c.w, c.h)
			}
		} else if err == nil {
			t.Errorf("parseDims(%q) expected error, got %dx%d", c.in, w, h)
		}
	}
}

// makeClip writes a synthetic 16:9 test clip of the given duration (seconds).
func makeClip(t *testing.T, ff, path string, durSec int) {
	t.Helper()
	mk := exec.Command(ff, "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=duration=%d:size=640x360:rate=10", durSec),
		"-pix_fmt", "yuv420p", path)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("make test clip: %v: %s", err, out)
	}
}

// TestGenerateTrickplay builds synthetic clips and asserts the sprite grid +
// manifest. ffmpeg-gated (skips without it, like the encode benchmark).
func TestGenerateTrickplay(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	cases := []struct {
		name               string
		durSec             int
		wantCount          int
		wantCols, wantRows int
	}{
		// fps=1/10 emits a frame at 0,10,20,… while t<dur → ceil(dur/10) frames.
		{"non_multiple_55s", 55, 6, 3, 2},   // ceil(55/10)=6
		{"exact_multiple_60s", 60, 6, 3, 2}, // ceil(60/10)=6 (NOT 7 — the off-by-one)
		{"short_clip_5s", 5, 1, 1, 1},       // 1x1 grid
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			clip := filepath.Join(dir, "clip.mp4")
			makeClip(t, ff, clip, c.durSec)

			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			m, err := GenerateTrickplay(ctx, ff, clip, 10, 240, float64(c.durSec))
			if err != nil {
				t.Fatalf("GenerateTrickplay: %v", err)
			}
			if m.Count != c.wantCount || m.Cols != c.wantCols || m.Rows != c.wantRows {
				t.Errorf("grid: count=%d cols=%d rows=%d; want %d/%d/%d",
					m.Count, m.Cols, m.Rows, c.wantCount, c.wantCols, c.wantRows)
			}
			if m.TileWidth != 240 {
				t.Errorf("tileWidth=%d; want 240", m.TileWidth)
			}
			if m.TileHeight < 130 || m.TileHeight > 140 {
				t.Errorf("tileHeight=%d; want ~135 (16:9)", m.TileHeight)
			}
			if m.IntervalSec != 10 {
				t.Errorf("intervalSec=%v; want 10 (no cap at this size)", m.IntervalSec)
			}
			if fi, err := os.Stat(TrickplaySpritePath(clip, 240)); err != nil || fi.Size() == 0 {
				t.Errorf("sprite not written: %v", err)
			}
			m2, ok := ReadCachedTrickplay(clip, 240)
			if !ok || m2.Count != m.Count || m2.TileHeight != m.TileHeight || m2.Cols != m.Cols {
				t.Errorf("ReadCachedTrickplay mismatch: ok=%v got=%+v want=%+v", ok, m2, m)
			}
			// Stale media (newer mtime) must invalidate the cache.
			future := time.Now().Add(2 * time.Hour)
			if err := os.Chtimes(clip, future, future); err == nil {
				if _, ok := ReadCachedTrickplay(clip, 240); ok {
					t.Error("ReadCachedTrickplay returned stale sprite after media mtime bumped")
				}
			}
		})
	}
}
