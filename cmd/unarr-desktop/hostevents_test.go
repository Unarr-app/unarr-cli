package main

import (
	"strings"
	"testing"
)

// wevtutilSample is the shape of `wevtutil qe Application /f:text /rd:true`: one
// "Event[N]:" header per event, CRLF line ends, localized free text.
const wevtutilSample = "Event[0]:\r\n  Log Name: Application\r\n  Source: Application Error\r\n  Event ID: 1000\r\n" +
	"  Description: \r\nFaulting application name: unarr.exe, version: 1.11.9.0\r\n" +
	"Event[1]:\r\n  Log Name: Application\r\n  Source: Application Error\r\n  Event ID: 1000\r\n" +
	"  Description: \r\nFaulting application name: OneDrive.exe\r\n" +
	"Event[2]:\r\n  Log Name: Application\r\n  Source: Windows Error Reporting\r\n  Event ID: 1001\r\n" +
	"  Description: \r\nNombre del evento: APPCRASH  P1: UNARR.EXE — señal\r\n"

// TestKeepEventsKeepsOnlyTheDaemonsOwn: other applications' crashes are noise in
// an unarr report, and the match must not depend on how WER capitalises names.
func TestKeepEventsKeepsOnlyTheDaemonsOwn(t *testing.T) {
	got := keepEvents(wevtutilSample, "unarr", hostEventsMaxBytes)
	if len(got) != 2 {
		t.Fatalf("kept %d events, want 2 (unarr.exe and UNARR.EXE):\n%q", len(got), got)
	}
	for _, ev := range got {
		if strings.Contains(ev, "OneDrive") {
			t.Errorf("another application's crash was kept:\n%s", ev)
		}
		if strings.ContainsRune(ev, '\r') {
			t.Errorf("CR left in event text:\n%q", ev)
		}
		for i := 0; i < len(ev); i++ {
			if ev[i] >= 0x80 {
				t.Fatalf("non-ASCII byte 0x%x in kept event:\n%s", ev[i], ev)
			}
		}
	}
	if all := keepEvents(wevtutilSample, "", hostEventsMaxBytes); len(all) != 3 {
		t.Errorf("an empty needle kept %d events, want every one (3)", len(all))
	}
}

// TestKeepEventsRespectsTheBudget: the context is never trimmed, so the event
// section must stop at its budget — and still say something when one event alone
// is larger than it.
func TestKeepEventsRespectsTheBudget(t *testing.T) {
	got := keepEvents(wevtutilSample, "", 120)
	total := 0
	for _, ev := range got {
		total += len(ev)
	}
	if len(got) != 1 || !strings.Contains(got[0], "[event truncated]") {
		t.Fatalf("an over-budget first event should be kept truncated, got %d events:\n%q", len(got), got)
	}
	if total > 120+len("\n[event truncated]") {
		t.Errorf("events use %d bytes, budget 120", total)
	}
}

// The queries themselves: a typo degrades into "unavailable" on a machine nobody
// can reproduce, so their shape is asserted here, on every platform.
func TestEventQueriesCarryTheirIDsAndWindow(t *testing.T) {
	const stamp = "2026-09-16T04:52:28.000Z"
	for _, tc := range []struct {
		name  string
		xpath string
		ids   []string
	}{
		{"crash", crashEventsXPath(stamp), []string{"EventID=1000", "EventID=1001", "EventID=1002"}},
		{"low memory", lowMemoryXPath(stamp), []string{"Microsoft-Windows-Resource-Exhaustion-Detector"}},
		{"host down", hostDownXPath(stamp), []string{
			"EventID=1074", "EventID=6006", "EventID=6008",
			"EventID=41", "Microsoft-Windows-Kernel-Power",
		}},
	} {
		if !strings.Contains(tc.xpath, "TimeCreated[@SystemTime>='"+stamp+"']") {
			t.Errorf("%s query does not bound the time window: %s", tc.name, tc.xpath)
		}
		if strings.Count(tc.xpath, "[") != strings.Count(tc.xpath, "]") ||
			strings.Count(tc.xpath, "(") != strings.Count(tc.xpath, ")") {
			t.Errorf("%s query is unbalanced: %s", tc.name, tc.xpath)
		}
		for _, id := range tc.ids {
			if !strings.Contains(tc.xpath, id) {
				t.Errorf("%s query does not ask for %s: %s", tc.name, id, tc.xpath)
			}
		}
	}
}
