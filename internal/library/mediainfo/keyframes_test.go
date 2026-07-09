package mediainfo

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestKeyframeSidecarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(media, []byte("fake media"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Miss before any write.
	if _, ok := ReadCachedKeyframes(media); ok {
		t.Fatal("expected cache miss before write")
	}

	kfs := []float64{0, 6.006, 12.012, 18.5}
	if err := WriteCachedKeyframes(media, kfs); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Sidecar lives in .unarr next to the media.
	if _, err := os.Stat(filepath.Join(dir, ".unarr", "movie.mkv.copyseg.json")); err != nil {
		t.Fatalf("sidecar not at expected path: %v", err)
	}

	got, ok := ReadCachedKeyframes(media)
	if !ok {
		t.Fatal("expected cache hit after write")
	}
	if len(got) != len(kfs) {
		t.Fatalf("got %d keyframes, want %d", len(got), len(kfs))
	}
	for i := range kfs {
		if got[i] != kfs[i] {
			t.Errorf("kf[%d] = %v, want %v", i, got[i], kfs[i])
		}
	}
}

func TestKeyframeSidecarStaleOnMediaChange(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(media, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteCachedKeyframes(media, []float64{0, 6}); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadCachedKeyframes(media); !ok {
		t.Fatal("expected fresh cache hit")
	}

	// Replacing the media (newer mtime) must invalidate the sidecar so the
	// keyframe table is re-indexed against the new content.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(media, future, future); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadCachedKeyframes(media); ok {
		t.Error("stale sidecar (media newer than cache) must be treated as a miss")
	}
}

func TestWriteCachedKeyframesRejectsEmpty(t *testing.T) {
	if err := WriteCachedKeyframes(filepath.Join(t.TempDir(), "x.mkv"), nil); err == nil {
		t.Error("writing an empty keyframe index must error, not cache garbage")
	}
}

func TestCopyVODEligibleCodec(t *testing.T) {
	cases := map[string]bool{
		"h264": true, "avc": true, "avc1": true, "H264": true, " h264 ": true,
		"hevc": false, "h265": false, "av1": false, "vp9": false, "": false,
	}
	for codec, want := range cases {
		if got := CopyVODEligibleCodec(codec); got != want {
			t.Errorf("CopyVODEligibleCodec(%q)=%v want %v", codec, got, want)
		}
	}
}
