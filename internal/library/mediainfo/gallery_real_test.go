package mediainfo

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestGalleryReal is a manual end-to-end harness against a REAL media library.
// It is skipped unless GALLERY_DIR is set, so it never runs in CI.
//
//	GALLERY_DIR=/mnt/nas/peliculas go test ./internal/library/mediainfo/ \
//	  -run TestGalleryReal -v -timeout 30m
//
// It surveys every video file (embedded subs via ffprobe + discovered sidecars),
// then actually extracts WebVTT for one representative of each kind and checks the
// output is a valid, non-empty WEBVTT document.
func TestGalleryReal(t *testing.T) {
	dir := os.Getenv("GALLERY_DIR")
	if dir == "" {
		t.Skip("set GALLERY_DIR to run the real-gallery survey")
	}
	ffprobe := envOr("FFPROBE", "ffprobe")
	ffmpeg := envOr("FFMPEG", "ffmpeg")

	videoExt := map[string]bool{".mkv": true, ".mp4": true, ".avi": true, ".m4v": true, ".webm": true, ".mov": true, ".ts": true}
	var videos []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.Contains(p, "/.unarr/") || strings.Contains(p, "/.Trash") || strings.Contains(p, "/@eaDir/") {
			return nil
		}
		if videoExt[strings.ToLower(filepath.Ext(p))] {
			videos = append(videos, p)
		}
		return nil
	})
	sort.Strings(videos)
	t.Logf("found %d video files under %s", len(videos), dir)

	type cat struct {
		embTextCodecs  map[string]int // codec → count of files
		embBitmap      map[string]int
		extCodecs      map[string]int
		filesEmbText   []string
		filesEmbBitmap []string
		filesExt       []string
		errs           int
	}
	c := cat{embTextCodecs: map[string]int{}, embBitmap: map[string]int{}, extCodecs: map[string]int{}}

	for _, v := range videos {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		mi, err := ExtractMediaInfo(ctx, ffprobe, v)
		cancel()
		if err != nil {
			c.errs++
			t.Logf("PROBE ERR %s: %v", filepath.Base(v), err)
			continue
		}
		var sawEmbText, sawEmbBitmap, sawExt bool
		for _, s := range mi.Subtitles {
			codec := strings.ToLower(s.Codec)
			switch {
			case s.External:
				c.extCodecs[codec]++
				sawExt = true
			case IsTextSubtitleCodec(codec):
				c.embTextCodecs[codec]++
				sawEmbText = true
			default:
				c.embBitmap[codec]++
				sawEmbBitmap = true
			}
		}
		if sawEmbText {
			c.filesEmbText = append(c.filesEmbText, v)
		}
		if sawEmbBitmap {
			c.filesEmbBitmap = append(c.filesEmbBitmap, v)
		}
		if sawExt {
			c.filesExt = append(c.filesExt, v)
		}
	}

	t.Logf("=== CENSUS ===")
	t.Logf("probe errors: %d", c.errs)
	t.Logf("embedded TEXT codecs (files w/ track): %v", c.embTextCodecs)
	t.Logf("embedded BITMAP codecs (burn-in only): %v", c.embBitmap)
	t.Logf("external SIDECAR codecs: %v", c.extCodecs)
	t.Logf("files w/ embedded text: %d | w/ embedded bitmap: %d | w/ external sidecar: %d",
		len(c.filesEmbText), len(c.filesEmbBitmap), len(c.filesExt))

	// --- Real extraction checks ---
	validVTT := func(b []byte) bool {
		return len(b) > 0 && strings.HasPrefix(strings.TrimSpace(string(b)), "WEBVTT")
	}

	// Embedded text: extract index 0 of the first such file.
	if len(c.filesEmbText) > 0 {
		f := c.filesEmbText[0]
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		out, err := ExtractSubtitleVTT(ctx, ffmpeg, f, 0)
		cancel()
		if err != nil || !validVTT(out) {
			t.Errorf("EMBEDDED extract FAILED for %s: err=%v len=%d", filepath.Base(f), err, len(out))
		} else {
			t.Logf("EMBEDDED extract OK: %s → %d bytes WebVTT", filepath.Base(f), len(out))
		}
	}

	// External sidecar: find one and extract it via the path-addressed function.
	if len(c.filesExt) > 0 {
		f := c.filesExt[0]
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		mi, _ := ExtractMediaInfo(ctx, ffprobe, f)
		cancel()
		var subPath, lang string
		for _, s := range mi.Subtitles {
			if s.External {
				subPath, lang = s.Path, s.Lang
				break
			}
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
		out, err := ExtractExternalSubtitleVTT(ctx2, ffmpeg, subPath, lang)
		cancel2()
		if err != nil || !validVTT(out) {
			t.Errorf("EXTERNAL extract FAILED for %s: err=%v len=%d", filepath.Base(subPath), err, len(out))
		} else {
			t.Logf("EXTERNAL extract OK: %s (lang=%s) → %d bytes WebVTT", filepath.Base(subPath), lang, len(out))
		}
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// TestGalleryExtractAllSidecars extracts EVERY discovered sidecar in the gallery
// and reports any that fail — the real proof the external path is robust across
// formats/charsets. Skipped unless GALLERY_DIR is set.
func TestGalleryExtractAllSidecars(t *testing.T) {
	dir := os.Getenv("GALLERY_DIR")
	if dir == "" {
		t.Skip("set GALLERY_DIR")
	}
	ffmpeg := envOr("FFMPEG", "ffmpeg")
	var subs []SubtitleTrack
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.Contains(p, "/.unarr/") || strings.Contains(p, "/.Trash") || strings.Contains(p, "/@eaDir/") {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if videoOf(ext) {
			subs = append(subs, DiscoverSidecarSubtitles(p)...)
		}
		return nil
	})
	// Dedupe by path.
	seen := map[string]bool{}
	var uniq []SubtitleTrack
	for _, s := range subs {
		if !seen[s.Path] {
			seen[s.Path] = true
			uniq = append(uniq, s)
		}
	}
	t.Logf("discovered %d unique sidecar subtitle files", len(uniq))
	fails := 0
	for _, s := range uniq {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		out, err := ExtractExternalSubtitleVTT(ctx, ffmpeg, s.Path, s.Lang)
		cancel()
		ok := len(out) > 0 && strings.HasPrefix(strings.TrimSpace(string(out)), "WEBVTT")
		if err != nil || !ok {
			fails++
			t.Errorf("FAIL %s (lang=%s codec=%s): err=%v len=%d", filepath.Base(s.Path), s.Lang, s.Codec, err, len(out))
		} else {
			t.Logf("OK   %s (lang=%s codec=%s) → %d bytes", filepath.Base(s.Path), s.Lang, s.Codec, len(out))
		}
	}
	if fails > 0 {
		t.Errorf("%d/%d sidecar extractions failed", fails, len(uniq))
	} else {
		t.Logf("all %d sidecar extractions produced valid WebVTT", len(uniq))
	}
}

func videoOf(ext string) bool {
	switch ext {
	case ".mkv", ".mp4", ".avi", ".m4v", ".webm", ".mov", ".ts":
		return true
	}
	return false
}
