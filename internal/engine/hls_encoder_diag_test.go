package engine

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSanitizeEncoderLine(t *testing.T) {
	const secret = "s3cr3t-token"
	for _, line := range []string{
		"[https @ 000001] HTTP error 410 Gone https://cdn.example/dld/" + secret + "/a.mkv",
		"Error opening input: https://cdn.example/a.mkv?token=" + secret,
		"failed for 'http://u:" + secret + "@host/x'",
	} {
		if got := sanitizeEncoderLine(line); strings.Contains(got, secret) {
			t.Errorf("sanitizeEncoderLine(%q) leaked the secret: %q", line, got)
		}
	}
	got := sanitizeEncoderLine("[hls @ 00000252dac6aec0] Failed to open segment 'init.mp4'")
	if got != "[hls] Failed to open segment 'init.mp4'" {
		t.Errorf("address not stripped: %q", got)
	}
	if long := sanitizeEncoderLine(strings.Repeat("é", 500)); len([]rune(long)) != encoderTailLineCap {
		t.Errorf("line not capped on a rune boundary: %d runes", len([]rune(long)))
	}
}

func TestEncoderTailKeepsTheEndWithoutRepeats(t *testing.T) {
	s := &HLSSession{}
	s.recordEncoderLine("oldest")
	for i := 0; i < encoderTailLines; i++ {
		s.recordEncoderLine(strconv.Itoa(i))
		s.recordEncoderLine(strconv.Itoa(i)) // a repeat must not use up a slot
	}
	tail := s.EncoderErrorTail()
	if strings.Contains(tail, "oldest") {
		t.Errorf("the oldest line should have been dropped: %q", tail)
	}
	if got := strings.Count(tail, " | ") + 1; got != encoderTailLines {
		t.Errorf("kept %d lines, want %d: %q", got, encoderTailLines, tail)
	}
	s.resetTranscodeStats()
	if got := s.EncoderErrorTail(); got != "" {
		t.Errorf("a new ffmpeg inherited the old tail: %q", got)
	}
}

func TestSourceRejectedOnlyForRemoteSources(t *testing.T) {
	const line = "[https @ 01] Server returned 4XX Client Error, but not one of 40{0,1,3,4}"
	local := &HLSSession{}
	local.recordEncoderLine(line)
	if local.SourceRejected() {
		t.Error("a local session cannot be refused by a remote server")
	}
	remote := &HLSSession{cfg: HLSSessionConfig{SourceURL: "https://cdn.example/a.mkv"}}
	remote.recordEncoderLine("Server returned 200 OK")
	if remote.SourceRejected() {
		t.Error("a successful status is not a rejection")
	}
	remote.recordEncoderLine(line)
	if !remote.SourceRejected() {
		t.Error("a 4xx from the remote server should mark the source as rejected")
	}
	remote.resetTranscodeStats()
	if remote.SourceRejected() {
		t.Error("the verdict must belong to the current ffmpeg only")
	}
}

// Meaningful on the Windows CI leg, where filepath.Join produces backslashes.
func TestHLSPlaylistArgHasForwardSlashes(t *testing.T) {
	dir := t.TempDir()
	probe := &StreamProbe{Width: 1920, Height: 1080, DurationSec: 60}
	for name, args := range map[string][]string{
		"encode": buildHLSFFmpegArgsAt(HLSSessionConfig{SourcePath: "in.mkv"}, probe, dir, 0, 0),
		"copy":   buildHLSCopyArgs(HLSSessionConfig{SourcePath: "in.mkv"}, probe, dir),
	} {
		playlist := ""
		for _, arg := range args {
			if strings.HasSuffix(arg, ".m3u8") {
				playlist = arg
			}
		}
		if playlist == "" {
			t.Fatalf("%s: no playlist in args: %v", name, args)
		}
		if strings.Contains(playlist, `\`) {
			t.Errorf("%s: playlist %q has a backslash: ffmpeg would write init.mp4 into the cwd", name, playlist)
		}
		if filepath.Dir(filepath.FromSlash(playlist)) != filepath.Join(dir, "video") {
			t.Errorf("%s: playlist %q is not in the session's video dir", name, playlist)
		}
	}
}
