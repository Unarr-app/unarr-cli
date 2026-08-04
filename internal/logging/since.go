package logging

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// sinceLayouts are the absolute forms --since accepts, most specific first.
// The time-only ones mean "today at", which is what someone debugging a
// this-morning incident types.
var sinceLayouts = []string{
	time.RFC3339,
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"15:04:05",
	"15:04",
}

// relativeUnits extend Go's duration vocabulary with the units operators
// actually type at a log tool. time.ParseDuration stops at hours.
var relativeUnits = map[byte]time.Duration{
	'd': 24 * time.Hour,
	'w': 7 * 24 * time.Hour,
}

// ParseSince turns a --since value into an absolute cut-off. Accepts a
// relative age ("30m", "2h", "7d", "1w") or an absolute stamp (RFC3339,
// "2006-01-02 15:04:05", "2006-01-02", or a bare "15:04" meaning today).
// An empty value is not an error: it means "no time filter", and yields the
// zero Time that Query.Since documents as such.
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if d, ok := parseRelative(s); ok {
		if d < 0 {
			return time.Time{}, fmt.Errorf("--since %q cannot be negative", s)
		}
		return now.Add(-d), nil
	}
	for _, layout := range sinceLayouts {
		t, err := time.ParseInLocation(layout, s, now.Location())
		if err != nil {
			continue
		}
		return graftDate(t, now), nil
	}
	return time.Time{}, fmt.Errorf(
		"cannot read --since %q (want an age like \"30m\", \"2h\", \"7d\", or a date like \"2006-01-02 15:04\")", s)
}

// parseRelative reads an "ago" duration, falling back to the d/w units the
// stdlib does not know.
func parseRelative(s string) (time.Duration, bool) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, true
	}
	mult, ok := relativeUnits[s[len(s)-1]]
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil {
		return 0, false
	}
	return time.Duration(n * float64(mult)), true
}

// graftDate gives a time-only value ("15:04") today's date, so it lands in the
// log's timeline instead of year zero.
func graftDate(t, now time.Time) time.Time {
	if t.Year() != 0 {
		return t
	}
	return time.Date(now.Year(), now.Month(), now.Day(),
		t.Hour(), t.Minute(), t.Second(), 0, now.Location())
}
