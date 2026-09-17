//go:build windows

package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// hostEventsTimeout bounds each event-log query. A report is collected while a
// user waits on a notification; wevtutil answers in well under a second, and a
// hung event log service must not hold the report hostage.
const hostEventsTimeout = 10 * time.Second

// hostEventsSection renders what the Windows event log recorded since the run
// began: crashes, WER reports and hangs that name unarr, every low-memory
// warning (those list the top consumers themselves, whoever they were), and the
// machine's own going-down events.
//
// The last group is what separates the two deaths that read alike in the daemon
// log — both stop mid-sentence with no panic. 1074 names the process that asked
// for the shutdown, 6006 is the event log closing on a clean one, and 6008 /
// Kernel-Power 41 are the machine losing power or hanging under it. None of them
// present, with no crash event either, is itself the answer: the process was
// killed while the host stayed up.
func hostEventsSection(since time.Time) string {
	if since.IsZero() {
		since = time.Now().Add(-24 * time.Hour)
	}
	// A minute of margin: the run's StartedAt is stamped after the process began.
	since = since.Add(-time.Minute)
	stamp := since.UTC().Format("2006-01-02T15:04:05.000Z")

	var b strings.Builder
	fmt.Fprintf(&b, "host events since %s (Windows event log):\n", since.UTC().Format(time.RFC3339))
	crashes, cerr := queryEvents("Application", crashEventsXPath(stamp), "unarr")
	pressure, perr := queryEvents("System", lowMemoryXPath(stamp), "")
	hostDown, herr := queryEvents("System", hostDownXPath(stamp), "")
	writeEvents(&b, "application crash/hang naming unarr", crashes, cerr)
	writeEvents(&b, "low memory", pressure, perr)
	writeEvents(&b, "host shutdown/power", hostDown, herr)
	return b.String()
}

func writeEvents(b *strings.Builder, what string, events []string, err error) {
	switch {
	case err != nil:
		fmt.Fprintf(b, "  %s: unavailable (%s)\n", what, asciiOnly(err.Error()))
	case len(events) == 0:
		fmt.Fprintf(b, "  %s: none\n", what)
	default:
		fmt.Fprintf(b, "  %s: %d event(s)\n", what, len(events))
		for _, ev := range events {
			b.WriteString(ev)
			b.WriteString("\n")
		}
	}
}

func queryEvents(logName, xpath, needle string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), hostEventsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wevtutil", "qe", logName, "/q:"+xpath, "/f:text", "/rd:true", "/c:20")
	winproc.HideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return keepEvents(string(out), needle, hostEventsMaxBytes/2), nil
}
