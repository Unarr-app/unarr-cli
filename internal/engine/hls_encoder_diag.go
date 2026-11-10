package engine

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Long on purpose: the web keeps the first 500 characters of the report, and the
// cause comes BEFORE ffmpeg's generic "Terminating thread" lines, which newer
// versions print many of.
const (
	encoderTailLines   = 12
	encoderTailLineCap = 120
)

var (
	reEncoderURL     = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s'"]+`)
	reEncoderAddress = regexp.MustCompile(`\s@\s[0-9a-fA-F]+\]`)
)

// hlsPlaylistOutput returns the playlist path to hand ffmpeg's HLS muxer. The
// muxer finds the directory for -hls_fmp4_init_filename by searching for '/',
// so on a backslash Windows path it finds none and writes init.mp4 into the
// CURRENT WORKING DIRECTORY. Autostarted daemons run from System32, where that
// open fails: every encode dies at exit 0xfffffff3 (AVERROR(EACCES)).
func hlsPlaylistOutput(path string) string {
	return filepath.ToSlash(path)
}

// isSourceRejectedLine spots a remote server refusing the source. Exit codes
// cannot carry this: each HTTP status maps to its own AVERROR, and they all
// reach the watchdog as a local "transcode_failed" otherwise.
func isSourceRejectedLine(line string) bool {
	l := strings.ToLower(line)
	return strings.Contains(l, "server returned 4") || strings.Contains(l, "server returned 5")
}

// sanitizeEncoderLine prepares an ffmpeg stderr line for leaving the machine.
// URLs go entirely, not only the session's own: a debrid refresh means the line
// may name an earlier link, and those carry tokens in the path or the query.
func sanitizeEncoderLine(line string) string {
	line = reEncoderURL.ReplaceAllString(line, "<url>")
	line = reEncoderAddress.ReplaceAllString(line, "]")
	if r := []rune(line); len(r) > encoderTailLineCap {
		line = string(r[:encoderTailLineCap])
	}
	return line
}

// recordEncoderLine keeps the end of the current ffmpeg's stderr: the exit code
// alone cannot say which file or stream failed, and the daemon log stays on the
// user's machine.
func (s *HLSSession) recordEncoderLine(line string) {
	clean := sanitizeEncoderLine(line)
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if s.cfg.SourceURL != "" && isSourceRejectedLine(line) {
		s.sourceRejected = true
	}
	if n := len(s.encoderTail); n > 0 && s.encoderTail[n-1] == clean {
		return
	}
	s.encoderTail = append(s.encoderTail, clean)
	if len(s.encoderTail) > encoderTailLines {
		s.encoderTail = s.encoderTail[1:]
	}
}

// EncoderErrorTail returns the last sanitized stderr lines of the current ffmpeg.
func (s *HLSSession) EncoderErrorTail() string {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return strings.Join(s.encoderTail, " | ")
}

// SourceRejected reports whether a remote server refused the current ffmpeg's
// source, which makes its failure the provider's and not this machine's.
func (s *HLSSession) SourceRejected() bool {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.sourceRejected
}
