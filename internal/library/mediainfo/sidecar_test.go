package mediainfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsTextSubtitleCodec(t *testing.T) {
	text := []string{"subrip", "srt", "ass", "ssa", "webvtt", "mov_text", "text", "SubRip", " ASS "}
	bitmap := []string{"hdmv_pgs_subtitle", "dvd_subtitle", "dvb_subtitle", "", "  ", "weirdcodec"}
	for _, c := range text {
		if !IsTextSubtitleCodec(c) {
			t.Errorf("IsTextSubtitleCodec(%q) = false, want true", c)
		}
	}
	for _, c := range bitmap {
		if IsTextSubtitleCodec(c) {
			t.Errorf("IsTextSubtitleCodec(%q) = true, want false", c)
		}
	}
}

func TestSubtitleCachePath(t *testing.T) {
	got := subtitleCachePath("/movies/Foo Bar.mkv", 3)
	want := filepath.Join("/movies", ".unarr", "Foo Bar.mkv.s3.vtt")
	if got != want {
		t.Errorf("subtitleCachePath = %q, want %q", got, want)
	}
}

func TestThumbnailCachePath(t *testing.T) {
	cases := []struct {
		pos   float64
		width int
		want  string
	}{
		{84.0, 320, "Foo.mkv.t84w320.jpg"},
		{84.3, 320, "Foo.mkv.t84w320.jpg"}, // rounds to whole seconds
		{84.6, 320, "Foo.mkv.t85w320.jpg"},
		{-5, 320, "Foo.mkv.t0w320.jpg"}, // negative clamps to 0
	}
	for _, c := range cases {
		got := thumbnailCachePath("/m/Foo.mkv", c.pos, c.width)
		want := filepath.Join("/m", ".unarr", c.want)
		if got != want {
			t.Errorf("thumbnailCachePath(%.1f,%d) = %q, want %q", c.pos, c.width, got, want)
		}
	}
}

func TestSidecarDirIsPerFolder(t *testing.T) {
	// Two files with the SAME basename in different dirs must not collide.
	a := subtitleCachePath("/a/Movie.mkv", 0)
	b := subtitleCachePath("/b/Movie.mkv", 0)
	if a == b {
		t.Errorf("same-basename files in different dirs collided: %q", a)
	}
	if filepath.Base(filepath.Dir(a)) != ".unarr" {
		t.Errorf("sidecar not in .unarr dir: %q", a)
	}
}

func TestSidecarFresh(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "m.mkv")
	cache := filepath.Join(dir, "m.cache")
	if err := os.WriteFile(media, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No cache file yet → not fresh.
	if sidecarFresh(cache, media) {
		t.Error("missing cache reported fresh")
	}

	// Cache newer than media → fresh.
	if err := os.WriteFile(cache, []byte("vtt"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(cache, future, future); err != nil {
		t.Fatal(err)
	}
	if !sidecarFresh(cache, media) {
		t.Error("cache newer than media reported stale")
	}

	// Media re-downloaded (newer than cache) → stale.
	newer := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(media, newer, newer); err != nil {
		t.Fatal(err)
	}
	if sidecarFresh(cache, media) {
		t.Error("cache older than media reported fresh")
	}

	// Missing media → not fresh (don't serve a sidecar for a vanished file).
	if sidecarFresh(cache, filepath.Join(dir, "gone.mkv")) {
		t.Error("missing media reported fresh")
	}
}

func TestWriteSidecarAtomicAndRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", ".unarr", "x.s0.vtt")

	if err := writeSidecar(p, nil); err == nil {
		t.Error("writeSidecar accepted empty data")
	}

	data := []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nhi\n")
	if err := writeSidecar(p, data); err != nil {
		t.Fatalf("writeSidecar: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != string(data) {
		t.Errorf("written sidecar mismatch: %q err=%v", got, err)
	}
	// No leftover temp file.
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file not cleaned up")
	}
	if !strings.HasSuffix(p, ".vtt") {
		t.Errorf("unexpected path: %q", p)
	}
}
