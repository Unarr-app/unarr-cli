package engine

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestCopyVODSmoke exercises the real COPY-VOD code path (indexKeyframes →
// planCopySegments → renderVideoPlaylistCopyVOD → generateCopySegment) against
// a real local media file, dumping the manifest + segments to a directory a
// static server can serve for an in-browser seek check.
//
// Opt-in (needs ffmpeg/ffprobe + a real file), e.g.:
//
//	UNARR_VOD_SMOKE=/path/to/file.mkv UNARR_VOD_OUT=/tmp/vodgo \
//	  go test ./internal/engine/ -run TestCopyVODSmoke -v
func TestCopyVODSmoke(t *testing.T) {
	src := os.Getenv("UNARR_VOD_SMOKE")
	if src == "" {
		t.Skip("set UNARR_VOD_SMOKE=<media file> to run the COPY-VOD smoke")
	}
	out := os.Getenv("UNARR_VOD_OUT")
	if out == "" {
		out = filepath.Join(os.TempDir(), "vodgo")
	}
	ffmpeg := "ffmpeg"
	ffprobe := "ffprobe"
	if p := os.Getenv("UNARR_FFMPEG"); p != "" {
		ffmpeg = p
	}
	if p := os.Getenv("UNARR_FFPROBE"); p != "" {
		ffprobe = p
	}

	ctx := context.Background()
	probe, err := ProbeFile(ctx, ffprobe, src)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("probe: codec=%s audio=%s dur=%.1fs", probe.VideoCodec, probe.AudioCodec, probe.DurationSec)

	tmpDir := filepath.Join(out, "session")
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(filepath.Join(tmpDir, "video"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &HLSSession{
		cfg: HLSSessionConfig{
			SessionID:  "smoketest01",
			SourcePath: src,
			VideoCopy:  true,
			AudioIndex: -1,
			Transcode:  TranscodeRuntime{FFmpegPath: ffmpeg, FFprobePath: ffprobe},
		},
		probe:       probe,
		tmpDir:      tmpDir,
		durationSec: probe.DurationSec,
		readyCh:     make(chan struct{}),
	}

	if !startCopyVOD(ctx, s) {
		t.Fatalf("startCopyVOD returned false (codec %q not eligible or index failed)", probe.VideoCodec)
	}
	t.Logf("copy-vod: %d segments, starts[0..3]=%v", s.segmentCount, s.copySegStarts[:minInt(4, len(s.copySegStarts))])

	// Write the manifest where a player expects it.
	manifestPath := filepath.Join(tmpDir, "video", "index.m3u8")
	if err := os.WriteFile(manifestPath, []byte(s.manifestVideo), 0o644); err != nil {
		t.Fatal(err)
	}

	// Generate the first N segments + a deep one (simulate a seek) so the
	// browser test can play start→boundary and jump mid-file. UNARR_VOD_ALL=1
	// generates every segment (for a native-Safari seek-anywhere test).
	var gen []int
	if os.Getenv("UNARR_VOD_ALL") == "1" {
		for i := 0; i < s.segmentCount; i++ {
			gen = append(gen, i)
		}
	} else {
		gen = []int{0, 1, 2, 3, 4}
		if s.segmentCount > 20 {
			gen = append(gen, s.segmentCount/2, s.segmentCount/2+1, s.segmentCount-1)
		}
	}
	for _, idx := range gen {
		if idx >= s.segmentCount {
			continue
		}
		if err := s.generateCopySegment(ctx, idx); err != nil {
			t.Fatalf("generateCopySegment(%d): %v", idx, err)
		}
	}
	t.Logf("manifest + %d segments written to %s/video", len(gen), tmpDir)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestCopyVODHTTPServe drives the REAL agent HTTP handler (StreamServer.hlsHandler
// → ServeVideoPlaylist / ServeSegment copy-vod branches + the .ts route) end to
// end: register a copy-vod session, then GET master.m3u8 / video/index.m3u8 /
// a deep segment over HTTP exactly as a browser would. This is the live-stack
// agent path minus the web's agent selection. Opt-in like TestCopyVODSmoke.
func TestCopyVODHTTPServe(t *testing.T) {
	src := os.Getenv("UNARR_VOD_SMOKE")
	if src == "" {
		t.Skip("set UNARR_VOD_SMOKE=<media file> to run the COPY-VOD HTTP serve test")
	}
	ffmpeg, ffprobe := "ffmpeg", "ffprobe"
	if p := os.Getenv("UNARR_FFMPEG"); p != "" {
		ffmpeg = p
	}
	if p := os.Getenv("UNARR_FFPROBE"); p != "" {
		ffprobe = p
	}
	ctx := context.Background()
	probe, err := ProbeFile(ctx, ffprobe, src)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	tmpDir := filepath.Join(os.TempDir(), "vodhttp", "session")
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(filepath.Join(tmpDir, "video"), 0o755); err != nil {
		t.Fatal(err)
	}
	const sid = "httptest0001"
	s := &HLSSession{
		cfg: HLSSessionConfig{
			SessionID: sid, SourcePath: src, VideoCopy: true, AudioIndex: -1,
			Transcode: TranscodeRuntime{FFmpegPath: ffmpeg, FFprobePath: ffprobe},
		},
		probe: probe, tmpDir: tmpDir, durationSec: probe.DurationSec,
		readyCh: make(chan struct{}),
	}
	if !startCopyVOD(ctx, s) {
		t.Fatalf("startCopyVOD false (codec %q)", probe.VideoCodec)
	}

	ss := NewStreamServer(0, 1)
	ss.SetRequireStreamToken(false) // drop the token segment for a simpler test URL
	ss.HLS().Register(s)
	srv := httptest.NewServer(http.HandlerFunc(ss.hlsHandler))
	defer srv.Close()

	get := func(path string) (int, string, http.Header) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}

	// master.m3u8
	if code, body, _ := get("/hls/" + sid + "/master.m3u8"); code != 200 || !strings.Contains(body, "video/index.m3u8") {
		t.Fatalf("master: code=%d body=%q", code, body)
	}
	// media playlist — must be the copy-vod MPEG-TS VOD manifest.
	code, pl, _ := get("/hls/" + sid + "/video/index.m3u8")
	if code != 200 {
		t.Fatalf("index.m3u8 code=%d", code)
	}
	for _, want := range []string{"#EXT-X-PLAYLIST-TYPE:VOD", "#EXT-X-ENDLIST", "seg-0.ts"} {
		if !strings.Contains(pl, want) {
			t.Fatalf("index.m3u8 missing %q:\n%s", want, pl[:minInt(300, len(pl))])
		}
	}
	if strings.Contains(pl, "EXT-X-MAP") || strings.Contains(pl, ".m4s") {
		t.Fatalf("copy-vod manifest must not reference fMP4 init/.m4s")
	}
	// init.mp4 must 404 (no fMP4 init in copy-vod).
	if code, _, _ := get("/hls/" + sid + "/video/init.mp4"); code != 404 {
		t.Errorf("init.mp4 should 404 in copy-vod, got %d", code)
	}
	// A DEEP segment (the "jump to minute X" case) generated on demand over HTTP.
	deep := s.segmentCount / 2
	code, _, hdr := get("/hls/" + sid + "/video/seg-" + strconv.Itoa(deep) + ".ts")
	if code != 200 {
		t.Fatalf("deep seg-%d.ts code=%d", deep, code)
	}
	if ct := hdr.Get("Content-Type"); ct != "video/mp2t" {
		t.Errorf("deep segment Content-Type=%q want video/mp2t", ct)
	}
	// It must exist on disk now + carry the absolute PTS of its boundary.
	segFile := s.copySegPath(deep)
	if fi, err := os.Stat(segFile); err != nil || fi.Size() == 0 {
		t.Fatalf("seg-%d.ts not produced on disk", deep)
	}
	t.Logf("HTTP serve OK: %d-seg copy-vod VOD manifest, deep seg-%d.ts (%.0f–%.0fs) served as video/mp2t",
		s.segmentCount, deep, s.copySegStarts[deep], s.copySegStarts[deep+1])
}
