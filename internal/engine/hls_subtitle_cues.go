package engine

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// vttCue is one WebVTT cue on the session timeline.
type vttCue struct {
	start, end time.Duration
	settings   string // cue settings after the end timestamp, verbatim
	text       string // may span lines
}

// id is a stable identifier for the cue, emitted as its WebVTT cue-identifier
// line. The player dedupes on it when it tops a partially loaded track up:
// windows are published out of playback order, so "whatever the track lacks"
// can no longer be told apart by position, and the player rewrites cue TIMES
// for the viewer's subtitle offset, so times can't key it either.
func (c vttCue) id() string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(c.text))
	return fmt.Sprintf("%d-%d-%08x", c.start.Milliseconds(), c.end.Milliseconds(), h.Sum32())
}

var vttTimingRe = regexp.MustCompile(`^\s*((?:\d+:)?\d{2}:\d{2}[.,]\d{3})\s+-->\s+((?:\d+:)?\d{2}:\d{2}[.,]\d{3})(.*)$`)

// parseVTTCues extracts the cues of a WebVTT document. Header, NOTE and STYLE
// blocks, malformed timings and empty cues are skipped.
func parseVTTCues(vtt []byte) []vttCue {
	text := strings.TrimPrefix(string(vtt), "\xef\xbb\xbf") // UTF-8 BOM
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var cues []vttCue
	for _, block := range strings.Split(text, "\n\n") {
		if cue, ok := parseVTTBlock(block); ok {
			cues = append(cues, cue)
		}
	}
	return cues
}

func parseVTTBlock(block string) (vttCue, bool) {
	lines := strings.Split(strings.Trim(block, "\n"), "\n")
	for i, line := range lines {
		m := vttTimingRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		start, okS := parseVTTStamp(m[1])
		end, okE := parseVTTStamp(m[2])
		body := strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		if !okS || !okE || end <= start || body == "" {
			return vttCue{}, false
		}
		return vttCue{start: start, end: end, settings: strings.TrimSpace(m[3]), text: body}, true
	}
	return vttCue{}, false
}

// parseVTTStamp reads "[HH:]MM:SS.mmm".
func parseVTTStamp(stamp string) (time.Duration, bool) {
	parts := strings.Split(strings.Replace(stamp, ",", ".", 1), ":")
	if len(parts) == 2 {
		parts = append([]string{"0"}, parts...)
	}
	h, errH := strconv.Atoi(parts[0])
	m, errM := strconv.Atoi(parts[1])
	sec, errS := strconv.ParseFloat(parts[2], 64)
	if errH != nil || errM != nil || errS != nil {
		return 0, false
	}
	ms := int64(h)*3_600_000 + int64(m)*60_000 + int64(sec*1000+0.5)
	return time.Duration(ms) * time.Millisecond, true
}

func formatVTTStamp(d time.Duration) string {
	ms := d.Milliseconds()
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3_600_000, ms/60_000%60, ms/1000%60, ms%1000)
}

// vttProgressNote prefixes the comment block carrying the progress value a
// served sidecar is current as of. In the body rather than a response header: a
// cross-origin player could not read a header without it being CORS-exposed.
const vttProgressNote = "NOTE progress="

// renderVTT writes cues as a WebVTT document, each with its identifier line.
// progress < 0 leaves the progress note out.
func renderVTT(cues []vttCue, progress int) []byte {
	var b strings.Builder
	b.WriteString("WEBVTT\n")
	if progress >= 0 {
		b.WriteString("\n" + vttProgressNote + strconv.Itoa(progress) + "\n")
	}
	for _, c := range cues {
		b.WriteString("\n" + c.id() + "\n")
		b.WriteString(formatVTTStamp(c.start) + " --> " + formatVTTStamp(c.end))
		if c.settings != "" {
			b.WriteString(" " + c.settings)
		}
		b.WriteString("\n" + c.text + "\n")
	}
	return []byte(b.String())
}
