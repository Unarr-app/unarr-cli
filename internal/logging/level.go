// Package logging owns the daemon log file: how it is written (size-based
// rotation, so a 24/7 NAS install cannot fill its disk), how severity is
// spelled, and how `unarr logs` reads it back.
package logging

import (
	"fmt"
	"strings"
)

// Level is the severity of a log line, ordered least to most severe so a
// filter is a plain comparison.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// DefaultLevel is what an install that never set log_level filters at.
const DefaultLevel = LevelInfo

var levelNames = [...]string{"debug", "info", "warn", "error"}

// String returns the canonical config spelling of a level.
func (l Level) String() string {
	if l < LevelDebug || int(l) >= len(levelNames) {
		return DefaultLevel.String()
	}
	return levelNames[l]
}

// Enabled reports whether a line of severity l survives a min filter.
func (l Level) Enabled(min Level) bool { return l >= min }

// LevelNames lists the accepted canonical spellings, for help text and shell
// completion. Returned as a fresh slice so a caller cannot mutate the table.
func LevelNames() []string { return append([]string(nil), levelNames[:]...) }

// ParseLevel maps a config value or a --log-level flag onto a Level. Empty is
// not an error: it means "unset", which resolves to DefaultLevel. The synonyms
// are the words people actually type; refusing them would be pedantry.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return DefaultLevel, nil
	case "debug", "trace", "verbose":
		return LevelDebug, nil
	case "info", "information", "notice":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error", "err", "fatal":
		return LevelError, nil
	}
	return DefaultLevel, fmt.Errorf("unknown log level %q (want %s)", s, strings.Join(levelNames[:], ", "))
}

// classifyMarkers maps a severity onto the words that betray it in an
// already-written line, most severe first.
var classifyMarkers = []struct {
	level Level
	words []string
}{
	{LevelError, []string{"error", "fatal", "panic"}},
	{LevelWarn, []string{"warn", "warning"}},
	{LevelDebug, []string{"debug", "trace"}},
}

// Classify guesses the severity of a line that is already on disk. The daemon
// logs free-form text through log.Printf — there is no severity FIELD to read —
// so we look for the markers the code actually emits ("[usenet] WARNING: …",
// "Error: …", `"level":"error"`) and treat anything untagged as info, which is
// what a plain operational line is. Heuristic on purpose: `unarr logs --level`
// is a reading aid, not a contract.
func Classify(line string) Level {
	low := strings.ToLower(line)
	for _, m := range classifyMarkers {
		for _, w := range m.words {
			if hasMarker(line, low, w) {
				return m.level
			}
		}
	}
	return LevelInfo
}

// hasMarker reports whether word appears as a severity TAG rather than as
// incidental prose: bracketed, or followed by the punctuation every logging
// convention uses, or shouted in upper case. Requiring one of those keeps a
// line like "no errors found" out of the error bucket.
func hasMarker(line, lower, word string) bool {
	for _, form := range []string{"[" + word + "]", word + ":", word + "=", "=" + word, `"` + word + `"`} {
		if strings.Contains(lower, form) {
			return true
		}
	}
	return strings.Contains(line, strings.ToUpper(word))
}
