package main

// What the host itself recorded about the daemon's death.
//
// Two field crash reports (2026-09-14, windows) described daemons that died with
// no panic, no fatal line and no shutdown in their own logs — the signature of a
// process killed from outside: an antivirus, Windows Error Reporting after an
// access violation in a DLL, or the low-memory killer. The daemon cannot log its
// own TerminateProcess, but Windows keeps its side of the story in the event log
// (Application Error 1000, WER 1001, Application Hang 1002, and the
// Resource-Exhaustion-Detector's low-memory events). The report context carries
// the matching entries so the next such report says who pulled the trigger.
//
// The query is per-platform (hostevents_windows.go); the parsing below is plain
// text handling, kept platform-neutral so it is tested everywhere.

import "strings"

// hostEventsMaxBytes caps what the event sections add to a report. The report
// context is never trimmed, so this budget comes straight out of the log tail.
const hostEventsMaxBytes = 4096

// keepEvents splits `wevtutil qe /f:text` output into events and keeps, in the
// order given (newest first with /rd:true), those that mention needle — any
// case; every event when needle is empty — until maxBytes is spent. An event
// that alone exceeds the budget is truncated rather than dropped: the first
// lines (source, date, id) are the ones that matter.
func keepEvents(raw, needle string, maxBytes int) []string {
	var kept []string
	used := 0
	for _, ev := range splitEvents(raw) {
		if needle != "" && !strings.Contains(strings.ToLower(ev), strings.ToLower(needle)) {
			continue
		}
		ev = asciiOnly(ev)
		if used+len(ev) > maxBytes {
			if len(kept) == 0 {
				kept = append(kept, ev[:maxBytes]+"\n[event truncated]")
			}
			break
		}
		kept = append(kept, ev)
		used += len(ev)
	}
	return kept
}

// splitEvents cuts wevtutil's text format at each "Event[N]:" header line.
func splitEvents(raw string) []string {
	var events []string
	var cur strings.Builder
	for line := range strings.Lines(strings.ReplaceAll(raw, "\r\n", "\n")) {
		if strings.HasPrefix(line, "Event[") && cur.Len() > 0 {
			events = append(events, strings.TrimRight(cur.String(), "\n"))
			cur.Reset()
		}
		cur.WriteString(line)
	}
	if strings.TrimSpace(cur.String()) != "" {
		events = append(events, strings.TrimRight(cur.String(), "\n"))
	}
	return events
}

// asciiOnly keeps the report framing readable in a CP1252 viewer, like the rest
// of the report context: event text is localized, so anything outside ASCII is
// replaced rather than passed through as mojibake.
func asciiOnly(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\r':
		case r < 0x80:
			b.WriteRune(r)
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}
