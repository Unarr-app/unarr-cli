package mediainfo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression tests for the second review round (mediainfo side).

func regressionFfmpeg(t *testing.T) string {
	t.Helper()
	ff, ok := LocateFFmpeg("")
	if !ok {
		t.Skip("ffmpeg unavailable")
	}
	return ff
}

func regressionSubripMKV(t *testing.T, ffmpegPath string) string {
	t.Helper()
	dir := t.TempDir()
	srt := filepath.Join(dir, "in.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:02,000\nHola subrip\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mkv := filepath.Join(dir, "subrip.mkv")
	out, err := exec.Command(ffmpegPath, "-nostdin", "-loglevel", "error", //nolint:gosec // test fixture build
		"-i", srt, "-c:s", "srt", "-y", mkv).CombinedOutput()
	if err != nil {
		t.Skipf("cannot build subrip fixture: %v: %s", err, out)
	}
	return mkv
}

// ffmpeg 8's ass muxer refuses `-c:s copy` of a subrip stream. That refusal is
// the NORMAL decline for f=ass on a non-ass track and must surface as
// ErrNotStyledSubtitle (informative), not as a generic extraction error that an
// operator would read as an incident.
func TestExtractSubtitleASSSubripDeclinesTyped(t *testing.T) {
	ff := regressionFfmpeg(t)
	mkv := regressionSubripMKV(t, ff)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := ExtractSubtitleASS(ctx, ff, mkv, 0)
	if err == nil {
		t.Fatal("subrip → f=ass must not succeed")
	}
	if !errors.Is(err, ErrNotStyledSubtitle) {
		t.Errorf("want ErrNotStyledSubtitle, got: %v", err)
	}
}

// A font dump aborted mid-write (ctx deadline, or the client hanging up — the
// handler ctx derives from the request) leaves a PARTIAL file. Returning it as
// success would cache a corrupt font for a day. The aborted run must fail even
// if bytes landed on disk.
func TestExtractFontAttachmentAbortedCtxFails(t *testing.T) {
	ff := regressionFfmpeg(t)
	mkv := regressionSubripMKV(t, ff) // any container: the ctx is dead before ffmpeg runs
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // aborted before the dump even starts — the extreme of "killed mid-write"
	_, err := ExtractFontAttachment(ctx, ff, mkv, 0, "font.ttf")
	if err == nil {
		t.Fatal("aborted ctx must not produce a successful font")
	}
	if !strings.Contains(err.Error(), "aborted") {
		t.Errorf("error should say the dump was aborted, got: %v", err)
	}
}

func TestIsASSSubtitlePath(t *testing.T) {
	for path, want := range map[string]bool{
		"movie.es.ass": true, "movie.SSA": true, "movie.es.srt": false,
		"movie.vtt": false, "noext": false,
	} {
		if got := IsASSSubtitlePath(path); got != want {
			t.Errorf("IsASSSubtitlePath(%q) = %v, want %v", path, got, want)
		}
	}
}

// The combined pass must hand back both representations, and the raw script it
// caches must pass the same authenticity bar as the on-demand path.
func TestExtractSubtitlesMultiEmitsBoth(t *testing.T) {
	ff := regressionFfmpeg(t)
	dir := t.TempDir()
	assIn := filepath.Join(dir, "in.ass")
	script := "[Script Info]\nScriptType: v4.00+\nPlayResX: 640\nPlayResY: 360\n\n[V4+ Styles]\nFormat: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\nStyle: Main,Arial,20,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,0,2,10,10,10,1\nStyle: Sign,Arial,20,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,0,2,10,10,10,1\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:02.00,Main,,0,0,0,,Hola ass\n"
	if err := os.WriteFile(assIn, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	mkv := filepath.Join(dir, "styled.mkv")
	out, err := exec.Command(ff, "-nostdin", "-loglevel", "error", //nolint:gosec // test fixture build
		"-i", assIn, "-c:s", "ass", "-y", mkv).CombinedOutput()
	if err != nil {
		t.Skipf("cannot build ass fixture: %v: %s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	vtt, ass, err := ExtractSubtitlesMulti(ctx, ff, mkv, []int{0}, []int{0})
	if err != nil {
		t.Fatalf("combined pass failed: %v", err)
	}
	if !strings.HasPrefix(string(vtt[0]), "WEBVTT") {
		t.Errorf("vtt output missing: %.40q", vtt[0])
	}
	if !strings.Contains(string(ass[0]), "Style: Sign") {
		t.Errorf("raw ass lost the authored style table: %.120q", ass[0])
	}
}
