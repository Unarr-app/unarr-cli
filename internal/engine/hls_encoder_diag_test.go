package engine

import (
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestSanitizeEncoderLine(t *testing.T) {
	const secret = "s3cr3t-token"
	for _, tc := range []struct{ line, source string }{
		{"[https @ 000001] HTTP error 410 Gone https://cdn.example/dld/" + secret + "/a.mkv", ""},
		{"Error opening input: https://cdn.example/a.mkv?token=" + secret, ""},
		{"failed for 'http://u:" + secret + "@host/x'", ""},
		// A decoded URL has spaces the generic pattern stops at; the session's
		// own URL is redacted by RedactSourceText first.
		{"Error opening input: https://cdn.example/x?file=My Movie.mkv&token=" + secret,
			"https://cdn.example/x?file=My%20Movie.mkv&token=" + secret},
	} {
		if got := sanitizeEncoderLine(tc.line, "", tc.source); strings.Contains(got, secret) {
			t.Errorf("sanitizeEncoderLine(%q) leaked the secret: %q", tc.line, got)
		}
	}
	for _, line := range []string{
		"[hls @ 00000252dac6aec0] Failed to open segment 'init.mp4'",
		"[hls @ 0x55d3c8a0e2c0] Failed to open segment 'init.mp4'",
	} {
		if got := sanitizeEncoderLine(line, ""); got != "[hls] Failed to open segment 'init.mp4'" {
			t.Errorf("address not stripped from %q: %q", line, got)
		}
	}
	for _, line := range []string{
		`Failed to open segment 'C:\Users\JDoe\AppData\Local\x\init.mp4'`,
		`Failed to open segment 'c:/users/jdoe/AppData/Local/x/init.mp4'`,
	} {
		got := sanitizeEncoderLine(line, `C:\Users\jdoe`)
		if strings.Contains(strings.ToLower(got), "jdoe") || !strings.Contains(got, "~") {
			t.Errorf("home not replaced in %q: %q", line, got)
		}
	}
	if long := sanitizeEncoderLine(strings.Repeat("é", 500), ""); len([]rune(long)) != encoderTailLineCap {
		t.Errorf("line not capped on a rune boundary: %d runes", len([]rune(long)))
	}
}

func TestEncoderTailKeepsTheNewestCause(t *testing.T) {
	s := &HLSSession{}
	s.recordEncoderLine("oldest warning")
	for i := 0; i < 10; i++ {
		line := "warning " + strconv.Itoa(i) + " " + strings.Repeat("x", 90)
		s.recordEncoderLine(line)
		s.recordEncoderLine(line) // a repeat must not use up a slot
	}
	s.recordEncoderLine("x264 [error]: invalid preset 'bogus'")
	s.recordEncoderLine("[vf#0:0 @ 0x55d8bc2eb2c0] Terminating thread with return code -22 (Invalid argument)")
	s.recordEncoderLine("[out#0/hls @ 0x55d8bc2ecac0] Nothing was written into output file")
	tail := s.EncoderErrorTail()
	if !strings.HasSuffix(tail, "x264 [error]: invalid preset 'bogus'") {
		t.Errorf("the cause must be the last kept line, generic chatter dropped: %q", tail)
	}
	if strings.Contains(tail, "oldest") || len(tail) > encoderTailBudget {
		t.Errorf("the tail must keep the newest lines within %d chars: %d %q", encoderTailBudget, len(tail), tail)
	}
	if got := buildEncoderTail([]string{"Conversion failed!"}, ""); got != "Conversion failed!" {
		t.Errorf("only generic lines: keep the last one rather than nothing, got %q", got)
	}
	s.resetTranscodeStats()
	if got := s.EncoderErrorTail(); got != "" {
		t.Errorf("a new ffmpeg inherited the old tail: %q", got)
	}
}

func TestIsHTTPExitCode(t *testing.T) {
	for code, want := range map[int]bool{
		0xcccfcb08: true,  // 403, Windows
		0xa7a7cb08: true,  // 410/429, Windows
		0xa7a7ca08: true,  // 500, Windows
		8:          true,  // any of them on unix: only the low byte survives
		0xfffffff3: false, // EACCES, Windows
		243:        false, // EACCES, unix
		234:        false, // EINVAL, unix
		1:          false,
	} {
		if got := isHTTPExitCode(code); got != want {
			t.Errorf("isHTTPExitCode(%#x) = %v, want %v", code, got, want)
		}
	}
}

// exitWith returns the *exec.ExitError of a process that exited with code.
func exitWith(t *testing.T, code int) error {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit "+strconv.Itoa(code))
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit "+strconv.Itoa(code))
	}
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected an exit error, got %v", err)
	}
	return err
}

func TestSourceRejected(t *testing.T) {
	const refusal = "[https @ 01] Server returned 4XX Client Error, but not one of 40{0,1,3,4}"
	httpExit, localExit := exitWith(t, 8), exitWith(t, 243)
	for _, tc := range []struct {
		name   string
		source string
		line   string
		exit   error
		want   bool
	}{
		{"remote refusal that killed ffmpeg", "https://cdn.example/a.mkv", refusal, httpExit, true},
		{"local file", "", refusal, httpExit, false},
		{"agent's own loopback server (usenet)", "http://127.0.0.1:41234/usenet/a.mkv", refusal, httpExit, false},
		{"localhost by name", "http://localhost:41234/a.mkv", refusal, httpExit, false},
		{"recovered refusal, later local fault", "https://cdn.example/a.mkv", refusal, localExit, false},
		{"HTTP exit without a refusal line", "https://cdn.example/a.mkv", "Server returned 200 OK", httpExit, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &HLSSession{cfg: HLSSessionConfig{SourceURL: tc.source}}
			s.recordEncoderLine(tc.line)
			s.exitErr = tc.exit
			if got := s.SourceRejected(); got != tc.want {
				t.Errorf("SourceRejected() = %v, want %v", got, tc.want)
			}
		})
	}
	s := &HLSSession{cfg: HLSSessionConfig{SourceURL: "https://cdn.example/a.mkv"}}
	s.recordEncoderLine(refusal)
	s.exitErr = httpExit
	s.resetTranscodeStats()
	if s.SourceRejected() {
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
