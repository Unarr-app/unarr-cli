package streaming

import (
	"strings"
	"testing"
	"time"

	"github.com/torrentclaw/unarr/internal/library/mediainfo"
)

// AnalyzeCompatibility — direct play happy paths.
func TestAnalyzeCompatibility_DirectPlayH264AAC(t *testing.T) {
	info := &mediainfo.MediaInfo{
		Video: &mediainfo.VideoInfo{Codec: "h264", BitDepth: 8},
		Audio: []mediainfo.AudioTrack{{Codec: "aac", Channels: 2}},
	}
	r := AnalyzeCompatibility(info)
	if !r.DirectPlay {
		t.Fatalf("h264+aac must be direct-playable, got %+v", r)
	}
	if len(r.Reasons) != 0 {
		t.Fatalf("direct play should have no reasons, got %v", r.Reasons)
	}
}

func TestAnalyzeCompatibility_DirectPlayVideoOnly(t *testing.T) {
	info := &mediainfo.MediaInfo{
		Video: &mediainfo.VideoInfo{Codec: "vp9", BitDepth: 8},
	}
	r := AnalyzeCompatibility(info)
	if !r.DirectPlay {
		t.Fatalf("video-only vp9 must be direct-playable, got %+v", r)
	}
}

// AnalyzeCompatibility — transcode required.
func TestAnalyzeCompatibility_TranscodeHEVC(t *testing.T) {
	info := &mediainfo.MediaInfo{
		Video: &mediainfo.VideoInfo{Codec: "hevc", BitDepth: 8},
		Audio: []mediainfo.AudioTrack{{Codec: "aac"}},
	}
	r := AnalyzeCompatibility(info)
	if r.DirectPlay {
		t.Fatalf("HEVC must NOT be direct-playable")
	}
	if !strings.Contains(strings.Join(r.Reasons, ";"), "hevc") {
		t.Fatalf("expected reason mentioning hevc, got %v", r.Reasons)
	}
}

func TestAnalyzeCompatibility_TranscodeHDR10bit(t *testing.T) {
	info := &mediainfo.MediaInfo{
		Video: &mediainfo.VideoInfo{Codec: "h264", BitDepth: 10, HDR: "HDR10"},
		Audio: []mediainfo.AudioTrack{{Codec: "aac"}},
	}
	r := AnalyzeCompatibility(info)
	if r.DirectPlay {
		t.Fatalf("10-bit HDR10 must NOT be direct-playable")
	}
}

func TestAnalyzeCompatibility_TranscodeEAC3Audio(t *testing.T) {
	info := &mediainfo.MediaInfo{
		Video: &mediainfo.VideoInfo{Codec: "h264", BitDepth: 8},
		Audio: []mediainfo.AudioTrack{{Codec: "eac3", Channels: 6}},
	}
	r := AnalyzeCompatibility(info)
	if r.DirectPlay {
		t.Fatalf("EAC3 audio must trigger transcode")
	}
	if r.VideoCompat != true {
		t.Fatalf("video stayed h264 — VideoCompat should still be true; got %+v", r)
	}
}

func TestAnalyzeCompatibility_NilGuard(t *testing.T) {
	r := AnalyzeCompatibility(nil)
	if r.DirectPlay {
		t.Fatal("nil MediaInfo must not be direct-playable")
	}
	r2 := AnalyzeCompatibility(&mediainfo.MediaInfo{Video: nil})
	if r2.DirectPlay {
		t.Fatal("MediaInfo without video must not be direct-playable")
	}
}

// ResolveQuality — fallback + table lookup.
func TestResolveQuality_FallbackTo1080p(t *testing.T) {
	got := ResolveQuality("")
	if got.Label != "1080p" {
		t.Fatalf("empty label fallback wrong: %s", got.Label)
	}
	got = ResolveQuality("garbage")
	if got.Label != "1080p" {
		t.Fatalf("unknown label fallback wrong: %s", got.Label)
	}
}

func TestResolveQuality_KnownLabels(t *testing.T) {
	cases := map[string]int{
		"480p":  480,
		"720p":  720,
		"1080p": 1080,
		"2160p": 2160,
	}
	for label, height := range cases {
		got := ResolveQuality(label)
		if got.MaxHeight != height {
			t.Errorf("ResolveQuality(%q).MaxHeight = %d want %d", label, got.MaxHeight, height)
		}
	}
}

// BuildFFmpegArgs — recipe shape verified by argv content.
func TestBuildFFmpegArgs_DirectPlayUsesCopy(t *testing.T) {
	report := CompatibilityReport{DirectPlay: true, VideoCompat: true, AudioCompat: true}
	args := BuildFFmpegArgs("/tmp/movie.mp4", report, StreamOptions{})
	joined := strings.Join(args, " ")

	want := []string{"-i /tmp/movie.mp4", "-c copy", "-movflags " + fragmentedMP4Movflags, "-f mp4", "pipe:1"}
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Fatalf("direct-play argv missing %q\n  got: %s", w, joined)
		}
	}
	if strings.Contains(joined, "libx264") {
		t.Fatalf("direct-play must NOT invoke libx264, got: %s", joined)
	}
}

func TestBuildFFmpegArgs_TranscodeUsesLibx264(t *testing.T) {
	report := CompatibilityReport{DirectPlay: false, VideoCompat: false, AudioCompat: true}
	args := BuildFFmpegArgs("/tmp/m.mkv", report, StreamOptions{Quality: "720p"})
	joined := strings.Join(args, " ")

	want := []string{
		"-c:v libx264",
		"scale=-2:720",
		"-b:v 3500000",
		"-c:a aac",
		"-b:a 128000",
		"-pix_fmt yuv420p",
		"-preset veryfast",
	}
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Fatalf("720p transcode argv missing %q\n  got: %s", w, joined)
		}
	}
}

func TestBuildFFmpegArgs_NVENCSwapsEncoder(t *testing.T) {
	report := CompatibilityReport{DirectPlay: false}
	args := BuildFFmpegArgs("/tmp/m.mkv", report, StreamOptions{HW: HWAccelNVENC})
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "-c:v h264_nvenc") {
		t.Fatalf("NVENC must use h264_nvenc, got: %s", joined)
	}
	if strings.Contains(joined, "-preset veryfast") {
		t.Fatalf("HW accel skips libx264 preset, got: %s", joined)
	}
}

func TestBuildFFmpegArgs_VAAPIInjectsHwaccelDecoder(t *testing.T) {
	report := CompatibilityReport{DirectPlay: false}
	args := BuildFFmpegArgs("/tmp/m.mkv", report, StreamOptions{HW: HWAccelVAAPI})
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "-hwaccel vaapi") {
		t.Fatalf("VAAPI must add -hwaccel vaapi, got: %s", joined)
	}
	if !strings.Contains(joined, "scale_vaapi") {
		t.Fatalf("VAAPI must use scale_vaapi filter, got: %s", joined)
	}
}

func TestBuildFFmpegArgs_StartOffsetEmitsSS(t *testing.T) {
	report := CompatibilityReport{DirectPlay: true}
	args := BuildFFmpegArgs("/tmp/m.mp4", report, StreamOptions{StartOffset: 65*time.Second + 500*time.Millisecond})
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "-ss 00:01:05.500") {
		t.Fatalf("expected -ss 00:01:05.500, got: %s", joined)
	}
}

// HWAccel encoders.
func TestHWAccel_VideoEncoder(t *testing.T) {
	cases := map[HWAccel]string{
		HWAccelNone:         "libx264",
		HWAccelUnset:        "libx264",
		HWAccelNVENC:        "h264_nvenc",
		HWAccelQSV:          "h264_qsv",
		HWAccelVAAPI:        "h264_vaapi",
		HWAccelVideoToolbox: "h264_videotoolbox",
	}
	for hw, want := range cases {
		if got := hw.VideoEncoder(); got != want {
			t.Errorf("%s.VideoEncoder() = %q want %q", hw, got, want)
		}
	}
}

func TestHWAccel_OnlyVAAPIHasDecoder(t *testing.T) {
	for _, h := range []HWAccel{HWAccelNone, HWAccelNVENC, HWAccelQSV, HWAccelVideoToolbox} {
		if h.HasDecoder() {
			t.Errorf("%s shouldn't claim HW decoder", h)
		}
	}
	if !HWAccelVAAPI.HasDecoder() {
		t.Error("VAAPI should claim HW decoder")
	}
}

// formatDuration — boundary cases.
func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "00:00:00.000"},
		{500 * time.Millisecond, "00:00:00.500"},
		{65 * time.Second, "00:01:05.000"},
		{2*time.Hour + 3*time.Minute + 7*time.Second + 250*time.Millisecond, "02:03:07.250"},
		{-time.Second, "00:00:00.000"},
	}
	for _, c := range cases {
		if got := formatDuration(c.in); got != c.want {
			t.Errorf("formatDuration(%v) = %q want %q", c.in, got, c.want)
		}
	}
}

// cappedBuffer — overflow keeps only the tail.
func TestCappedBuffer_KeepsTail(t *testing.T) {
	b := newCappedBuffer(10)
	b.Write([]byte("hello "))
	b.Write([]byte("world"))
	b.Write([]byte("!"))
	// "hello " + "world" + "!" = 12 bytes; cap 10 → keep last 10 = "llo world!".
	got := b.String()
	if got != "llo world!" {
		t.Fatalf("unexpected tail %q", got)
	}
}

func TestCappedBuffer_LargeSingleWrite(t *testing.T) {
	b := newCappedBuffer(5)
	b.Write([]byte("abcdefghij"))
	if got := b.String(); got != "fghij" {
		t.Fatalf("large write tail wrong: %q", got)
	}
}

// NewTranscoder rejects empty paths.
func TestNewTranscoder_RequiresBothBinaries(t *testing.T) {
	if _, err := NewTranscoder("", "/usr/bin/ffprobe"); err == nil {
		t.Error("expected error for empty ffmpeg path")
	}
	if _, err := NewTranscoder("/usr/bin/ffmpeg", ""); err == nil {
		t.Error("expected error for empty ffprobe path")
	}
	if _, err := NewTranscoder("/usr/bin/ffmpeg", "/usr/bin/ffprobe"); err != nil {
		t.Errorf("valid paths should not error: %v", err)
	}
}
