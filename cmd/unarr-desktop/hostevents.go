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

// The three event-log queries a report makes, as wevtutil XPath. They live here,
// outside the windows build tag, so their shape is tested on every platform —
// a typo in one of them degrades silently into "unavailable" in the one place
// nobody can reproduce. stamp is an event-log UTC timestamp
// (2006-01-02T15:04:05.000Z).
//
// crashEventsXPath: Application Error (1000), WER (1001), Application Hang
// (1002) — the daemon dying of an access violation or being reported by WER.
func crashEventsXPath(stamp string) string {
	return "*[System[(EventID=1000 or EventID=1001 or EventID=1002) and " +
		"TimeCreated[@SystemTime>='" + stamp + "']]]"
}

// lowMemoryXPath: the Resource-Exhaustion-Detector's warnings, which name the
// top memory consumers themselves.
func lowMemoryXPath(stamp string) string {
	return "*[System[Provider[@Name='Microsoft-Windows-Resource-Exhaustion-Detector'] and " +
		"TimeCreated[@SystemTime>='" + stamp + "']]]"
}

// hostDownXPath: the machine going down under the daemon. 1074 names the process
// that requested the shutdown, 6006 is the event log closing on a clean one,
// 6008 is the previous shutdown having been unexpected, and Kernel-Power 41 is
// the box losing power or hanging. A death with none of these and no crash event
// happened while the host stayed up — it was killed.
func hostDownXPath(stamp string) string {
	return "*[System[(EventID=1074 or EventID=6006 or EventID=6008 or " +
		"(EventID=41 and Provider[@Name='Microsoft-Windows-Kernel-Power'])) and " +
		"TimeCreated[@SystemTime>='" + stamp + "']]]"
}

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
