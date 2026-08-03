package logging

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Format is how `unarr logs` renders what it read.
type Format string

const (
	// FormatText prints the line as the daemon wrote it — the historical
	// behaviour, and what a human tailing a log wants.
	FormatText Format = "text"
	// FormatJSON prints one JSON object per line (json-lines), for piping into
	// jq or a log shipper on a headless box.
	FormatJSON Format = "json"
)

// DefaultFormat is what an install that never set log_format renders as.
const DefaultFormat = FormatText

// FormatNames lists the accepted spellings, for help text and completion.
func FormatNames() []string { return []string{string(FormatText), string(FormatJSON)} }

// ParseFormat maps a config value onto a Format. Empty means "unset" and
// resolves to DefaultFormat.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return DefaultFormat, nil
	case "text", "plain", "console":
		return FormatText, nil
	case "json", "jsonl", "json-lines", "ndjson":
		return FormatJSON, nil
	}
	return DefaultFormat, fmt.Errorf("unknown log format %q (want %s)", s, strings.Join(FormatNames(), ", "))
}

// Entry is one log line, split into the parts a filter or a JSON renderer
// needs. Raw is kept verbatim so text output is byte-identical to the file.
type Entry struct {
	Time    time.Time
	Level   Level
	Message string
	Raw     string
}

// entryJSON is the wire shape of a json-lines record. Time is omitted rather
// than emitted as a zero date when the source line carried no timestamp.
type entryJSON struct {
	Time    string `json:"time,omitempty"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// timeLayouts are the stamp shapes a daemon line can start with: Go's stdlib
// log default first (that is what log.Printf writes), then the ISO spellings
// used by anything piping through us.
var timeLayouts = []string{
	"2006/01/02 15:04:05",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02 15:04:05",
}

// ParseEntry splits a raw log line into time, severity and message. Every part
// is best-effort: an unrecognisable line still yields a usable Entry whose
// Message is the whole line, because dropping output the daemon produced would
// be a worse failure than showing it unparsed.
func ParseEntry(line string) Entry {
	e := Entry{Raw: line, Level: Classify(line), Message: line}
	if ts, rest, ok := splitTimestamp(line); ok {
		e.Time, e.Message = ts, rest
	}
	return e
}

// splitTimestamp peels a leading timestamp off a line, returning the remainder
// as the message.
func splitTimestamp(line string) (time.Time, string, bool) {
	for _, layout := range timeLayouts {
		if len(line) < len(layout) {
			continue
		}
		ts, err := time.ParseInLocation(layout, line[:len(layout)], time.Local)
		if err != nil {
			continue
		}
		return ts, strings.TrimSpace(line[len(layout):]), true
	}
	return time.Time{}, "", false
}

// Render turns an entry back into the one line to print, in this format.
func (f Format) Render(e Entry) string {
	if f != FormatJSON {
		return e.Raw
	}
	rec := entryJSON{Level: e.Level.String(), Message: e.Message}
	if !e.Time.IsZero() {
		rec.Time = e.Time.Format(time.RFC3339)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		// Marshalling three strings cannot realistically fail; fall back to the
		// raw line rather than swallowing output.
		return e.Raw
	}
	return string(b)
}
