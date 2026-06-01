package engine

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBitrateForQuality(t *testing.T) {
	cases := map[string]int{
		"2160p":   25_000_000,
		"1080p":   6_000_000,
		"720p":    3_500_000,
		"480p":    1_500_000,
		"unknown": 6_000_000,
		"":        6_000_000,
	}
	for q, want := range cases {
		if got := bitrateForQuality(q); got != want {
			t.Errorf("bitrateForQuality(%q) = %d, want %d", q, got, want)
		}
	}
}

func TestQualityHeight(t *testing.T) {
	cases := map[string]int{
		"2160p":   2160,
		"1080p":   1080,
		"720p":    720,
		"480p":    480,
		"":        0,
		"unknown": 0,
	}
	for q, want := range cases {
		if got := qualityHeight(q); got != want {
			t.Errorf("qualityHeight(%q) = %d, want %d", q, got, want)
		}
	}
}

func TestScaledDimensions(t *testing.T) {
	tests := []struct {
		name             string
		srcW, srcH, capH int
		wantW, wantH     int
	}{
		{"no_cap_returns_source", 1920, 1080, 0, 1920, 1080},
		{"under_cap_returns_source", 1280, 720, 1080, 1280, 720},
		{"4k_capped_to_1080", 3840, 2160, 1080, 1920, 1080},
		{"even_width_stays_even", 1003, 750, 720, 962, 720},
		{"odd_width_bumps_up", 1001, 700, 500, 716, 500},
		{"invalid_returns_default", 0, 0, 0, 1920, 1080},
		{"negative_returns_default", -10, 100, 0, 1920, 1080},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotW, gotH := scaledDimensions(tt.srcW, tt.srcH, tt.capH)
			if gotW != tt.wantW || gotH != tt.wantH {
				t.Errorf("scaledDimensions(%d,%d,%d) = (%d,%d), want (%d,%d)",
					tt.srcW, tt.srcH, tt.capH, gotW, gotH, tt.wantW, tt.wantH)
			}
		})
	}
}

func TestShortHLSID(t *testing.T) {
	if got := shortHLSID("abcdef1234567890"); got != "abcdef12" {
		t.Errorf("got %q, want abcdef12", got)
	}
	if got := shortHLSID("short"); got != "short" {
		t.Errorf("got %q, want short", got)
	}
	if got := shortHLSID(""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestHlsTmpDirRoot(t *testing.T) {
	root := hlsTmpDirRoot()
	if root == "" {
		t.Fatal("hlsTmpDirRoot returned empty")
	}
	if !strings.Contains(root, "hls-sessions") && !strings.Contains(root, "unarr-hls-sessions") {
		t.Errorf("expected path to contain hls-sessions, got %q", root)
	}
}

func TestRenderVideoPlaylist(t *testing.T) {
	out := renderVideoPlaylist(10.0, 3)
	required := []string{
		"#EXTM3U",
		"#EXT-X-VERSION:7",
		"#EXT-X-PLAYLIST-TYPE:VOD",
		`#EXT-X-MAP:URI="init.mp4"`,
		"seg-0.m4s",
		"seg-1.m4s",
		"seg-2.m4s",
		"#EXT-X-ENDLIST",
	}
	for _, want := range required {
		if !strings.Contains(out, want) {
			t.Errorf("playlist missing %q\n%s", want, out)
		}
	}
}

func TestRenderVideoPlaylistShortFinalSegment(t *testing.T) {
	// 9.5s total, 2s segments → 5 segs of 2/2/2/2/1.5
	segCount := segmentCountForDuration(9.5)
	out := renderVideoPlaylist(9.5, segCount)
	if !strings.Contains(out, "#EXTINF:1.500,") {
		t.Errorf("expected final segment 1.5s in playlist (segCount=%d), got:\n%s", segCount, out)
	}
}

func TestRenderMasterPlaylist(t *testing.T) {
	probe := &StreamProbe{
		Width:  1920,
		Height: 1080,
		SubtitleTracks: []ProbeSubtitleTrack{
			{Index: 0, Lang: "es", Codec: "subrip", Title: "Spanish"},
			{Index: 1, Lang: "en", Codec: "subrip", Title: "English", Forced: true},
			{Index: 2, Lang: "ja", Codec: "hdmv_pgs_subtitle"}, // bitmap, skipped
		},
	}
	out := renderMasterPlaylist(probe, "1080p")

	if !strings.HasPrefix(out, "#EXTM3U") {
		t.Errorf("must start with #EXTM3U, got:\n%s", out)
	}
	if !strings.Contains(out, "BANDWIDTH=6000000") {
		t.Errorf("expected 1080p bandwidth, got:\n%s", out)
	}
	if !strings.Contains(out, "RESOLUTION=1920x1080") {
		t.Errorf("expected 1920x1080 resolution, got:\n%s", out)
	}
	// Subtitles are NO LONGER embedded as HLS renditions — the web player
	// attaches them as external <track>s (served by /sub). The master playlist
	// must therefore carry no SUBTITLES group, no EXT-X-MEDIA, and no SUBTITLES
	// attribute on the video variant, even when the source has text subs.
	if strings.Contains(out, "SUBTITLES") {
		t.Errorf("subtitles must NOT be embedded in the manifest (served as external <track>), got:\n%s", out)
	}
	if strings.Contains(out, "EXT-X-MEDIA") {
		t.Errorf("no EXT-X-MEDIA rendition expected, got:\n%s", out)
	}
}

func TestRenderMasterPlaylistNoSubs(t *testing.T) {
	probe := &StreamProbe{Width: 1280, Height: 720}
	out := renderMasterPlaylist(probe, "720p")
	if strings.Contains(out, "SUBTITLES=") {
		t.Errorf("no subs should produce no SUBTITLES attr, got:\n%s", out)
	}
	if !strings.Contains(out, "BANDWIDTH=3500000") {
		t.Errorf("expected 720p bandwidth, got:\n%s", out)
	}
}

func TestHLSSessionRegistry(t *testing.T) {
	r := NewHLSSessionRegistry()
	if r.Get("missing") != nil {
		t.Error("Get on empty registry should return nil")
	}

	s1 := &HLSSession{cfg: HLSSessionConfig{SessionID: "a"}, lastTouch: time.Now()}
	r.Register(s1)
	if got := r.Get("a"); got != s1 {
		t.Errorf("Get(a) = %v, want %v", got, s1)
	}

	// Registering a different session evicts (and Closes) the previous one.
	s2 := &HLSSession{cfg: HLSSessionConfig{SessionID: "b"}, lastTouch: time.Now()}
	r.Register(s2)
	if r.Get("a") != nil {
		t.Error("registering different session should evict prior entries")
	}
	if r.Get("b") != s2 {
		t.Error("Get(b) should return s2")
	}

	r.Remove("b")
	if r.Get("b") != nil {
		t.Error("Remove should drop the session")
	}
}

func TestHLSSessionAccessors(t *testing.T) {
	probe := &StreamProbe{VideoCodec: "h264", Width: 1280, Height: 720}
	s := &HLSSession{
		cfg:           HLSSessionConfig{SessionID: "abcdef1234"},
		probe:         probe,
		manifestRoot:  "MASTER",
		manifestVideo: "VIDEO",
		durationSec:   42.5,
		lastTouch:     time.Now().Add(-1 * time.Hour),
	}
	if s.MasterPlaylist() != "MASTER" {
		t.Errorf("MasterPlaylist mismatch")
	}
	if s.VideoPlaylist() != "VIDEO" {
		t.Errorf("VideoPlaylist mismatch")
	}
	if s.DurationSeconds() != 42.5 {
		t.Errorf("DurationSeconds mismatch")
	}
	if s.Probe() != probe {
		t.Errorf("Probe mismatch")
	}

	old := s.lastTouch
	s.Touch()
	if !s.lastTouch.After(old) {
		t.Errorf("Touch did not advance lastTouch")
	}

	info := s.ProbeInfo()
	if info["videoCodec"] != "h264" || info["width"] != 1280 {
		t.Errorf("ProbeInfo missing fields: %v", info)
	}
}

func TestHLSSessionProbeInfoNil(t *testing.T) {
	s := &HLSSession{}
	info := s.ProbeInfo()
	if len(info) != 0 {
		t.Errorf("nil probe should produce empty info, got %v", info)
	}
}

func TestSweepIdle(t *testing.T) {
	r := NewHLSSessionRegistry()
	idleSession := &HLSSession{
		cfg:       HLSSessionConfig{SessionID: "old"},
		lastTouch: time.Now().Add(-2 * hlsSessionTTL),
	}
	r.Register(idleSession)
	if got := r.SweepIdle(); got != 1 {
		t.Errorf("SweepIdle = %d, want 1", got)
	}
	if r.Get("old") != nil {
		t.Errorf("idle session should have been removed")
	}
}

func TestCleanupHLSOrphanDirsMissingRoot(t *testing.T) {
	// Directory does not exist — should not error.
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "nonexistent"))
	if err := CleanupHLSOrphanDirs(); err != nil {
		t.Errorf("CleanupHLSOrphanDirs on missing root = %v, want nil", err)
	}
}

func TestValidSessionID(t *testing.T) {
	good := []string{
		"abc",
		"7b8c4f12-9d3e-4a1b-9c2f-aabbccddeeff",
		"ABC_123-xyz",
		strings.Repeat("a", 128),
	}
	bad := []string{
		"",
		"../etc/passwd",
		"foo/bar",
		"foo\\bar",
		"foo.bar",
		"with spaces",
		"with\nnewline",
		strings.Repeat("a", 129),
		"héctor", // non-ascii
	}
	for _, id := range good {
		if !validSessionID.MatchString(id) {
			t.Errorf("validSessionID rejected good id %q", id)
		}
	}
	for _, id := range bad {
		if validSessionID.MatchString(id) {
			t.Errorf("validSessionID accepted bad id %q", id)
		}
	}
}
