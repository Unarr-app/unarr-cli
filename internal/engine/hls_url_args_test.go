package engine

import (
	"strings"
	"testing"
)

// hueco #2 / 2b — buildHLSFFmpegArgsAt must feed a debrid URL straight to
// ffmpeg's -i with HTTP-resilience flags, and must NOT add those flags for a
// local file.
func TestBuildHLSFFmpegArgsFromURL(t *testing.T) {
	const url = "https://cdn.debrid.it/dl/abc/Movie.mkv"
	cfg := HLSSessionConfig{
		SessionID: "test",
		SourceURL: url,
		CacheID:   "deadbeef",
		Quality:   "720p",
		Transcode: TranscodeRuntime{
			FFmpegPath:  "/usr/bin/ffmpeg",
			FFprobePath: "/usr/bin/ffprobe",
			HWAccel:     HWAccelNone,
		},
	}
	probe := &StreamProbe{Width: 1920, Height: 1080, DurationSec: 100}
	args := buildHLSFFmpegArgsAt(cfg, probe, "/tmp/tmpdir", 0, 0)
	got := strings.Join(args, " ")

	for _, want := range []string{
		"-reconnect 1",
		"-reconnect_streamed 1",
		"-reconnect_delay_max 5",
		"-rw_timeout 30000000",
		"-i " + url,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("URL argv missing %q\n%s", want, got)
		}
	}
}

// A seek (startSec>0) on a URL source must keep BOTH the -ss input seek AND the
// HTTP-resilience flags, so a seek-restart re-opens the URL with a Range request
// instead of re-downloading from zero. (-ss before -i = input seek.)
func TestBuildHLSFFmpegArgsFromURLWithSeek(t *testing.T) {
	const url = "https://cdn.debrid.it/dl/abc/Movie.mkv"
	cfg := HLSSessionConfig{
		SessionID: "test",
		SourceURL: url,
		CacheID:   "deadbeef",
		Quality:   "720p",
		Transcode: TranscodeRuntime{
			FFmpegPath:  "/usr/bin/ffmpeg",
			FFprobePath: "/usr/bin/ffprobe",
			HWAccel:     HWAccelNone,
		},
	}
	probe := &StreamProbe{Width: 1920, Height: 1080, DurationSec: 100}
	got := strings.Join(buildHLSFFmpegArgsAt(cfg, probe, "/tmp/tmpdir", 5, 30), " ")

	for _, want := range []string{
		"-ss 30.000",   // input seek before -i
		"-reconnect 1", // resilience flags still present on a restart
		"-rw_timeout 30000000",
		"-i " + url,
		"-output_ts_offset 30.000", // PTS shift so the manifest numbering holds
	} {
		if !strings.Contains(got, want) {
			t.Errorf("seek+URL argv missing %q\n%s", want, got)
		}
	}
	// -ss must come before -i (fast input seek, not slow output seek).
	if strings.Index(got, "-ss 30.000") > strings.Index(got, "-i "+url) {
		t.Errorf("-ss must precede -i for input seek:\n%s", got)
	}
}

func TestBuildHLSFFmpegArgsLocalNoNetworkFlags(t *testing.T) {
	cfg := HLSSessionConfig{
		SessionID:  "test",
		SourcePath: "/tmp/test.mkv",
		Quality:    "720p",
		Transcode: TranscodeRuntime{
			FFmpegPath:  "/usr/bin/ffmpeg",
			FFprobePath: "/usr/bin/ffprobe",
			HWAccel:     HWAccelNone,
		},
	}
	probe := &StreamProbe{Width: 1920, Height: 1080, DurationSec: 100}
	got := strings.Join(buildHLSFFmpegArgsAt(cfg, probe, "/tmp/tmpdir", 0, 0), " ")

	if strings.Contains(got, "-reconnect") || strings.Contains(got, "-rw_timeout") {
		t.Errorf("local source must not carry HTTP-resilience flags: %s", got)
	}
	if !strings.Contains(got, "-i /tmp/test.mkv") {
		t.Errorf("local argv missing -i /tmp/test.mkv: %s", got)
	}
}

// sourceRef + cache-key identity: a URL session keys by CacheID, a local one by
// path. Guards the "re-plays of the same debrid content hit cache despite the
// URL changing" invariant.
func TestHLSSourceRefAndCacheID(t *testing.T) {
	urlCfg := HLSSessionConfig{SourceURL: "https://cdn/x.mkv", CacheID: "hash1"}
	if urlCfg.sourceRef() != "https://cdn/x.mkv" {
		t.Errorf("sourceRef = %q, want the URL", urlCfg.sourceRef())
	}
	localCfg := HLSSessionConfig{SourcePath: "/m/x.mkv"}
	if localCfg.sourceRef() != "/m/x.mkv" {
		t.Errorf("sourceRef = %q, want the path", localCfg.sourceRef())
	}

	c := &HLSCache{root: "/tmp/cache"}
	// Same CacheID + quality + audio → same key regardless of the (volatile) URL.
	k1 := c.KeyForID("hash1", "720p", -1, -1)
	k2 := c.KeyForID("hash1", "720p", -1, -1)
	if k1 != k2 {
		t.Errorf("KeyForID not stable: %q != %q", k1, k2)
	}
	if c.KeyForID("hash2", "720p", -1, -1) == k1 {
		t.Error("KeyForID collision across distinct ids")
	}
}

// Burn-in: a bitmap subtitle index routes the video through -filter_complex with
// scale2ref + overlay and maps [vout]; a nil / text / out-of-range index keeps
// the plain -vf path (text subs are served as WebVTT, never burned).
func TestBuildHLSFFmpegArgsBurnSubtitle(t *testing.T) {
	idx := func(n int) *int { return &n }
	base := func() HLSSessionConfig {
		return HLSSessionConfig{
			SessionID:  "burn",
			SourcePath: "/tmp/movie.mkv",
			Quality:    "1080p",
			Transcode: TranscodeRuntime{
				FFmpegPath:  "/usr/bin/ffmpeg",
				FFprobePath: "/usr/bin/ffprobe",
				HWAccel:     HWAccelNone,
			},
		}
	}
	probe := &StreamProbe{
		Width: 1920, Height: 1080, DurationSec: 100,
		SubtitleTracks: []ProbeSubtitleTrack{
			{Index: 0, Codec: "subrip"},            // text → not burnable
			{Index: 1, Codec: "hdmv_pgs_subtitle"}, // bitmap → burnable
		},
	}

	t.Run("nil = clean -vf path", func(t *testing.T) {
		got := strings.Join(buildHLSFFmpegArgsAt(base(), probe, "/tmp/d", 0, 0), " ")
		if strings.Contains(got, "-filter_complex") || strings.Contains(got, "overlay") {
			t.Errorf("no-burn argv must not overlay: %s", got)
		}
		if !strings.Contains(got, "-map 0:v:0") || !strings.Contains(got, "-vf") {
			t.Errorf("no-burn argv must -map 0:v:0 with -vf: %s", got)
		}
	})

	t.Run("bitmap index burns via filter_complex", func(t *testing.T) {
		cfg := base()
		cfg.BurnSubtitleIndex = idx(1)
		got := strings.Join(buildHLSFFmpegArgsAt(cfg, probe, "/tmp/d", 0, 0), " ")
		for _, want := range []string{"-filter_complex", "[0:s:1]", "scale2ref", "overlay", "-map [vout]"} {
			if !strings.Contains(got, want) {
				t.Errorf("burn argv missing %q: %s", want, got)
			}
		}
		if strings.Contains(got, "-map 0:v:0") {
			t.Errorf("burn argv must map [vout], not 0:v:0: %s", got)
		}
	})

	t.Run("text index is ignored (served as WebVTT)", func(t *testing.T) {
		cfg := base()
		cfg.BurnSubtitleIndex = idx(0) // subrip → not a bitmap track
		got := strings.Join(buildHLSFFmpegArgsAt(cfg, probe, "/tmp/d", 0, 0), " ")
		if strings.Contains(got, "overlay") || strings.Contains(got, "-filter_complex") {
			t.Errorf("text-sub burn must fall back to clean encode: %s", got)
		}
	})

	t.Run("out-of-range index is ignored", func(t *testing.T) {
		cfg := base()
		cfg.BurnSubtitleIndex = idx(9)
		got := strings.Join(buildHLSFFmpegArgsAt(cfg, probe, "/tmp/d", 0, 0), " ")
		if strings.Contains(got, "overlay") {
			t.Errorf("out-of-range burn must fall back to clean encode: %s", got)
		}
	})
}
