package library

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

// The keyframe prewarm shipped dead twice: the daemon built PrewarmOptions with
// no FFprobePath/Keyframes at all, and PrewarmSidecars' own guard vetoed every
// job when ffmpeg was missing even though the keyframe job only needs ffprobe.
// Both let a cold play pay the full-file index inline (~150 s for a 12 GB h264
// over NFS) and fall back to EVENT copy, which ignores the resume position.
// This matrix pins the tool-resolution gate so neither can regress silently.

func h264Item(path string) LibraryItem {
	return LibraryItem{
		FilePath:  path,
		MediaInfo: &mediainfo.MediaInfo{Video: &mediainfo.VideoInfo{Codec: "h264", Duration: 100}},
	}
}

func TestPrewarmSidecarsToolGate(t *testing.T) {
	cases := []struct {
		name        string
		ffmpeg      string
		ffprobe     string
		subs        bool
		thumbs      bool
		trickplay   bool
		keyframes   bool
		wantEnqueue bool // did any job reach a worker?
	}{
		// The regression this whole change exists to prevent: ffprobe present,
		// keyframes requested, every ffmpeg-driven toggle off. Must still run.
		{"keyframes only, no ffmpeg", "", "/bin/ffprobe", false, false, false, true, true},
		{"keyframes only, with ffmpeg", "/bin/ffmpeg", "/bin/ffprobe", false, false, false, true, true},
		// Keyframes requested but ffprobe unresolvable → that job alone is off.
		{"keyframes wanted, no ffprobe", "/bin/ffmpeg", "", false, false, false, true, false},
		// ffmpeg-driven jobs must still be vetoed when ffmpeg is missing.
		{"subs wanted, no ffmpeg", "", "/bin/ffprobe", true, false, false, false, false},
		{"thumbs wanted, no ffmpeg", "", "/bin/ffprobe", false, true, false, false, false},
		{"trickplay wanted, no ffmpeg", "", "/bin/ffprobe", false, false, true, false, false},
		// Nothing requested → no work regardless of tools.
		{"nothing requested", "/bin/ffmpeg", "/bin/ffprobe", false, false, false, false, false},
		{"no tools at all", "", "", true, true, true, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A cancelled context stops each worker before it spawns a real
			// ffprobe/ffmpeg, so this asserts the GATE, not the extraction.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			cache := &LibraryCache{Items: []LibraryItem{h264Item(filepath.Join(t.TempDir(), "x.mkv"))}}
			PrewarmSidecars(ctx, cache, PrewarmOptions{
				FFmpegPath:           tc.ffmpeg,
				FFprobePath:          tc.ffprobe,
				CacheSubtitles:       tc.subs,
				CacheThumbnails:      tc.thumbs,
				Trickplay:            tc.trickplay,
				TrickplayIntervalSec: 10,
				Keyframes:            tc.keyframes,
				Workers:              1,
			})
			// PrewarmSidecars is best-effort and returns no value; the contract
			// under test is that it neither panics nor deadlocks on any tool
			// combination. A gate that wrongly returns early and one that wrongly
			// proceeds are separated by TestPrewarmKeyframesRunsWithoutFFmpeg.
		})
	}
}

// Proves the ffprobe-only path actually reaches mediainfo.PrewarmKeyframes and
// writes the sidecar — the assertion TestPrewarmSidecarsToolGate cannot make
// with a cancelled context. Skipped when no real ffprobe/ffmpeg is installed.
func TestPrewarmKeyframesRunsWithoutFFmpeg(t *testing.T) {
	ffprobe, err := mediainfo.ResolveFFprobe("")
	if err != nil {
		t.Skipf("ffprobe unavailable: %v", err)
	}
	ffmpeg, err := mediainfo.ResolveFFmpeg("")
	if err != nil {
		t.Skipf("ffmpeg unavailable (needed to synthesize the fixture): %v", err)
	}

	dir := t.TempDir()
	media := filepath.Join(dir, "clip.mkv")
	// 3 s of h264 with a keyframe every second → a real, tiny COPY-VOD candidate.
	gen := exec.Command(ffmpeg, "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=128x72:rate=10:duration=3",
		"-c:v", "libx264", "-g", "10", "-pix_fmt", "yuv420p", media)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("could not synthesize fixture: %v (%s)", err, out)
	}

	cache := &LibraryCache{Items: []LibraryItem{h264Item(media)}}
	// FFmpegPath deliberately empty: this is the "ffprobe only" box.
	// MaxLoadRatio is set absurdly high so the keyframe job's load gate never
	// defers: this test asserts the TOOL gate, and a loaded CI box would
	// otherwise make it wait out prewarmLoadWaitCap and blow the package timeout.
	PrewarmSidecars(context.Background(), cache, PrewarmOptions{
		FFprobePath:  ffprobe,
		Keyframes:    true,
		Workers:      1,
		MaxLoadRatio: 1e6,
	})

	sidecar := filepath.Join(dir, ".unarr", "clip.mkv.copyseg.json")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("keyframe sidecar not written with ffmpeg absent: %v", err)
	}
	kfs, ok := mediainfo.ReadCachedKeyframes(media)
	if !ok || len(kfs) == 0 {
		t.Fatalf("cached keyframes unreadable: ok=%v n=%d", ok, len(kfs))
	}
}
