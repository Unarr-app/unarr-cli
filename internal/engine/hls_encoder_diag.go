package engine

import (
	"errors"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	encoderTailLines   = 12
	encoderTailLineCap = 120
	encoderRawLineCap  = 1024
	// The web keeps the first 500 characters of a session error; this leaves room
	// for the "transcode failed: exit status 0x…: " prefix.
	encoderTailBudget = 440
)

var (
	reEncoderURL     = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s'"]+`)
	reEncoderAddress = regexp.MustCompile(`\s@\s(?:0x)?[0-9a-fA-F]+\]`)
	// ffmpeg's shutdown chatter: printed on every failure, names no cause, and on
	// newer versions it is the LAST several lines, pushing the cause out.
	reEncoderGeneric = regexp.MustCompile(`(?i)terminating thread with return code|task finished with error code|nothing was written into output file|could not open encoder before eof|conversion failed`)
)

// hlsPlaylistOutput returns the playlist path to hand ffmpeg's HLS muxer. The
// muxer finds the directory for -hls_fmp4_init_filename by searching for '/',
// so on a backslash Windows path it finds none and writes init.mp4 into the
// CURRENT WORKING DIRECTORY. Autostarted daemons run from System32, where that
// open fails: every encode dies at exit 0xfffffff3 (AVERROR(EACCES)).
func hlsPlaylistOutput(path string) string {
	return filepath.ToSlash(path)
}

// isSourceRejectedLine spots a server refusing the source in ffmpeg's stderr.
func isSourceRejectedLine(line string) bool {
	l := strings.ToLower(line)
	return strings.Contains(l, "server returned 4") || strings.Contains(l, "server returned 5")
}

// isHTTPExitCode reports an ffmpeg exit caused by an HTTP status. Every
// AVERROR_HTTP_* is FFERRTAG(0xF8, …), so its low byte is 0x08 and no other
// ffmpeg error has that byte — also on unix, where only that byte survives.
// Measured on Windows: 403 → 0xcccfcb08, 410/429 → 0xa7a7cb08, 500 → 0xa7a7ca08.
func isHTTPExitCode(code int) bool {
	return code&0xff == 0x08
}

func isHTTPExit(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && isHTTPExitCode(exitErr.ExitCode())
}

// isRemoteSource reports whether rawURL points off this machine. Usenet streams
// through the agent's own loopback server, and its errors are ours to report.
func isRemoteSource(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// replaceHome swaps the user's home directory for "~", whichever separator and
// case ffmpeg printed it with: it carries the OS account name.
func replaceHome(line, home string) string {
	home = strings.TrimRight(strings.ReplaceAll(home, `\`, "/"), "/")
	if home == "" {
		return line
	}
	pattern := strings.ReplaceAll(regexp.QuoteMeta(home), "/", `[\\/]`)
	return regexp.MustCompile(`(?i)`+pattern).ReplaceAllString(line, "~")
}

// sanitizeEncoderLine prepares an ffmpeg stderr line for leaving the machine.
// The session's own URLs go through RedactSourceText (it also knows their
// decoded forms), then every remaining URL goes: after a debrid refresh the line
// may name an earlier link, and those carry tokens in the path or the query.
func sanitizeEncoderLine(line, home string, sourceURLs ...string) string {
	line = strings.ToValidUTF8(line, "")
	for _, u := range sourceURLs {
		line = RedactSourceText(line, u)
	}
	line = reEncoderURL.ReplaceAllString(line, "<url>")
	line = replaceHome(line, home)
	line = reEncoderAddress.ReplaceAllString(line, "]")
	if r := []rune(line); len(r) > encoderTailLineCap {
		line = string(r[:encoderTailLineCap])
	}
	return line
}

// recordEncoderLine keeps the end of the current ffmpeg's stderr: the exit code
// alone cannot say which file or stream failed, and the daemon log stays on the
// user's machine. Raw here; sanitized only when a failure is reported.
func (s *HLSSession) recordEncoderLine(line string) {
	if len(line) > encoderRawLineCap {
		line = line[:encoderRawLineCap]
	}
	rejected := isSourceRejectedLine(line)
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if rejected {
		s.sourceRejected = true
	}
	if n := len(s.encoderTail); n > 0 && s.encoderTail[n-1] == line {
		return
	}
	s.encoderTail = append(s.encoderTail, line)
	if len(s.encoderTail) > encoderTailLines {
		s.encoderTail = s.encoderTail[1:]
	}
}

// EncoderErrorTail returns the newest meaningful stderr lines of the current
// ffmpeg, sanitized, within what the web keeps.
func (s *HLSSession) EncoderErrorTail() string {
	s.statsMu.Lock()
	raw := slices.Clone(s.encoderTail)
	s.statsMu.Unlock()
	s.mu.Lock()
	live := s.liveURL
	s.mu.Unlock()
	home, _ := os.UserHomeDir()
	return buildEncoderTail(raw, home, s.cfg.SourceURL, live)
}

func buildEncoderTail(raw []string, home string, sourceURLs ...string) string {
	var kept []string
	size := 0
	for i := len(raw) - 1; i >= 0; i-- {
		if reEncoderGeneric.MatchString(raw[i]) {
			continue
		}
		line := sanitizeEncoderLine(raw[i], home, sourceURLs...)
		if len(kept) > 0 && size+len(line)+3 > encoderTailBudget {
			break
		}
		kept = append(kept, line)
		size += len(line) + 3
	}
	if len(kept) == 0 && len(raw) > 0 {
		kept = append(kept, sanitizeEncoderLine(raw[len(raw)-1], home, sourceURLs...))
	}
	slices.Reverse(kept)
	return strings.Join(kept, " | ")
}

// SourceRejected reports whether the current ffmpeg died BECAUSE a remote server
// refused its source: a refusal line, an HTTP exit, and a source off this
// machine. A refusal ffmpeg recovered from does not relabel a later local fault.
func (s *HLSSession) SourceRejected() bool {
	s.statsMu.Lock()
	seen := s.sourceRejected
	s.statsMu.Unlock()
	if !seen || !isRemoteSource(s.cfg.SourceURL) {
		return false
	}
	s.readyMu.Lock()
	exitErr := s.exitErr
	s.readyMu.Unlock()
	return isHTTPExit(exitErr)
}
