package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

// Windowed extraction must yield exactly what one whole-file pass yields: no cue
// lost or doubled at a window seam, none shifted. Needs real media:
//
//	UNARR_SUBS_SAMPLE=/path/to/file-with-text-subs.mkv go test -run WindowedSubtitlesMatch ./internal/engine/
func TestWindowedSubtitlesMatchWholeFilePass(t *testing.T) {
	sample := os.Getenv("UNARR_SUBS_SAMPLE")
	ffmpeg, err := exec.LookPath("ffmpeg")
	if sample == "" || err != nil {
		t.Skip("set UNARR_SUBS_SAMPLE (and have ffmpeg on PATH)")
	}
	ctx := context.Background()
	probe, err := ProbeFile(ctx, "ffprobe", sample)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	s := &HLSSession{tmpDir: t.TempDir(), probe: probe, durationSec: probe.DurationSec}
	s.cfg.SourcePath = sample
	s.cfg.Transcode.FFmpegPath = ffmpeg
	tracks := s.textSubtitleTracks()
	if len(tracks) == 0 {
		t.Skip("sample has no text subtitle track")
	}

	w := newSubtitleWindows("[t]", s.durationSec, s.extractSubtitleWindow)
	w.seek(s.durationSec / 2) // out of order on purpose
	w.run(ctx)

	track := tracks[0]
	whole := filepath.Join(s.tmpDir, "whole.vtt")
	out, err := exec.CommandContext(ctx, ffmpeg, "-y", "-nostdin", "-loglevel", "error", "-i", sample, //nolint:gosec // G204: test-only, operator-supplied sample.
		"-map", "0:s:"+strconv.Itoa(track), "-c:s", "webvtt", "-f", "webvtt", whole).CombinedOutput()
	if err != nil {
		t.Fatalf("whole-file pass: %v (%s)", err, out)
	}
	raw, err := os.ReadFile(whole) //nolint:gosec // G304: test temp dir.
	if err != nil {
		t.Fatal(err)
	}
	want, got := parseVTTCues(mediainfo.FilterVTTDrawingCues(raw)), w.snapshot(track)
	if len(want) == 0 {
		t.Fatal("whole-file pass produced no cues")
	}
	if !reflect.DeepEqual(got, want) {
		i := firstCueDiff(got, want)
		t.Fatalf("windowed: %d cues, whole file: %d cues; first difference at %d:\n got %+v\nwant %+v",
			len(got), len(want), i, got[i:min(i+2, len(got))], want[i:min(i+2, len(want))])
	}
}

func firstCueDiff(a, b []vttCue) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}
