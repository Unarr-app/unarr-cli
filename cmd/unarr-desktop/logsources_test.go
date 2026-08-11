package main

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubUnarr replaces the CLI with a table of argv → output, so the log
// collection can be driven without an `unarr` on PATH. An argv with no entry
// gets what a missing binary produces: no output, and the exec error.
//
// The call log is mutex-guarded because logSections reads its two sources
// concurrently — without it, `go test -race` fails on the stub rather than on
// anything under test.
func stubUnarr(t *testing.T, replies map[string]struct {
	out string
	err error
}) *stubCalls {
	t.Helper()
	calls := &stubCalls{}
	restore := runUnarrOutput
	t.Cleanup(func() { runUnarrOutput = restore })
	runUnarrOutput = func(args ...string) ([]byte, error) {
		key := strings.Join(args, " ")
		calls.record(key)
		r, ok := replies[key]
		if !ok {
			return nil, errors.New(`exec: "unarr": executable file not found in $PATH`)
		}
		return []byte(r.out), r.err
	}
	return calls
}

// stubCalls records the argvs the stub was asked for.
type stubCalls struct {
	mu   sync.Mutex
	seen []string
}

func (c *stubCalls) record(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, key)
}

func (c *stubCalls) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

type reply = struct {
	out string
	err error
}

// TestReportLogsCarryTheStartupLog is the whole point of the change: a Go panic
// never passes through log.SetOutput, so it lands in the launcher's boot log
// and NOT in unarr.log. Reports only ever carried unarr.log, which made every
// crash report structurally incapable of containing the crash.
func TestReportLogsCarryTheStartupLog(t *testing.T) {
	calls := stubUnarr(t, map[string]reply{
		"daemon logs":        {out: "2026/08/04 01:19:00 [cleanup] nothing to clean\n"},
		"daemon logs --boot": {out: "panic: runtime error: invalid memory address\n\ngoroutine 1 [running]:\n"},
	})

	body := string(collectReportLogs())
	if !strings.Contains(body, "panic: runtime error") {
		t.Fatalf("the report dropped the panic — that is the bug:\n%s", body)
	}
	if !strings.Contains(body, "[cleanup] nothing to clean") {
		t.Fatalf("the report dropped the daemon log:\n%s", body)
	}
	if got := calls.list(); len(got) != 2 {
		t.Fatalf("expected both log sources to be read, got calls %v", got)
	}
}

// TestReportLogsPutTheStartupLogLast: sendReport tails the assembled body one
// final time, so the section that survives a body over budget is the last one.
// The panic is worth more than another few KiB of DHT bookkeeping.
func TestReportLogsPutTheStartupLogLast(t *testing.T) {
	stubUnarr(t, map[string]reply{
		"daemon logs":        {out: "daemon-line\n"},
		"daemon logs --boot": {out: "boot-line\n"},
	})

	body := string(collectReportLogs())
	if strings.Index(body, "daemon-line") > strings.Index(body, "boot-line") {
		t.Fatalf("the startup log must come last so the final tail keeps it:\n%s", body)
	}
}

// TestLogSectionsReadConcurrently pins the property the doc comment claims and
// nothing else would catch. Each stub blocks until the OTHER one has started;
// serialised reads therefore deadlock and this test times out, while concurrent
// reads finish immediately. No sleeps, no wall-clock thresholds — the
// synchronisation IS the assertion.
func TestLogSectionsReadConcurrently(t *testing.T) {
	daemonIn, bootIn := make(chan struct{}), make(chan struct{})
	restore := runUnarrOutput
	t.Cleanup(func() { runUnarrOutput = restore })
	runUnarrOutput = func(args ...string) ([]byte, error) {
		if len(args) == 3 { // daemon logs --boot
			close(bootIn)
			<-daemonIn
			return []byte("boot\n"), nil
		}
		close(daemonIn)
		<-bootIn
		return []byte("daemon\n"), nil
	}

	done := make(chan []logSection, 1)
	go func() { done <- logSections() }()
	select {
	case got := <-done:
		if len(got) != 2 {
			t.Fatalf("want both sections, got %d", len(got))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("logSections serialised its two reads: each source waits for the other to start, " +
			"so a sequential implementation can never finish. See the doc comment on logSections.")
	}
}

// TestReportLogsWithoutABootLog covers every ordinary reason the second source
// is absent — a systemd install (its startup output is in the journal), a
// daemon only ever run in the foreground, or a CLI too old to know --boot. None
// is a fault, so none may leave an error in the body a developer reads.
func TestReportLogsWithoutABootLog(t *testing.T) {
	cases := map[string]reply{
		"old CLI":         {out: "Error: unknown flag: --boot", err: errors.New("exit status 1")},
		"systemd install": {out: "Error: --boot has no file to read here", err: errors.New("exit status 1")},
		"never started":   {out: "Error: no startup log yet at /x/unarr.boot.log", err: errors.New("exit status 1")},
		"empty file":      {out: "", err: nil},
		"whitespace only": {out: "\n \n", err: nil},
	}
	for name, bootReply := range cases {
		t.Run(name, func(t *testing.T) {
			stubUnarr(t, map[string]reply{
				"daemon logs":        {out: "daemon-line\n"},
				"daemon logs --boot": bootReply,
			})

			body := string(collectReportLogs())
			if body != "daemon-line\n" {
				t.Fatalf("a missing startup log must leave the body exactly as it was, got:\n%q", body)
			}
		})
	}
}

// TestReportLogsPlaceholderSurvives: an empty read still produces an actionable
// placeholder rather than an empty report — and it says the RIGHT thing, which
// depends on why the read came back empty.
//
// The two cases are opposites and used to share one message. "The CLI ran and
// printed nothing" really is the foreground-daemon case, and "install it as a
// service" is the fix. "The CLI could not be run" is not: a field crash report
// carried `No logs available. (exit status 0xc0000142)` plus that same advice,
// on a box where the agent WAS a service and where the loader had killed the
// collector before main(). It blamed the user's setup for a broken binary.
func TestReportLogsPlaceholderSurvives(t *testing.T) {
	// A CLI that ran fine and had nothing to say.
	t.Run("no logs to show", func(t *testing.T) {
		stubUnarr(t, map[string]reply{"daemon logs": {out: ""}})

		body := string(collectReportLogs())
		if !strings.Contains(body, "No logs available.") {
			t.Fatalf("lost the no-logs placeholder:\n%s", body)
		}
		if !strings.Contains(body, "unarr daemon install") {
			t.Fatalf("the placeholder must still say how to get logs:\n%s", body)
		}
		if strings.Contains(body, "=====") {
			t.Fatalf("a lone section must render bare, without headers:\n%s", body)
		}
	})

	// A player-only box: no CLI on disk at all, so the exec itself fails.
	t.Run("the collector could not run", func(t *testing.T) {
		stubUnarr(t, map[string]reply{})

		body := string(collectReportLogs())
		if !strings.Contains(body, "executable file not found") {
			t.Fatalf("the placeholder must carry the reason there are no logs:\n%s", body)
		}
		if !strings.Contains(body, "COULD NOT READ THE LOGS") {
			t.Fatalf("a failed collection must say the COLLECTION failed:\n%s", body)
		}
		if strings.Contains(body, "unarr daemon install") {
			t.Fatalf("a collector that could not start is not a foreground daemon — "+
				"this advice sends the reader at the wrong fault:\n%s", body)
		}
		if strings.Contains(body, "=====") {
			t.Fatalf("a lone section must render bare, without headers:\n%s", body)
		}
	})
}

// TestReportLogsBudgets: each source is trimmed to its own budget, so a noisy
// daemon log (the funnel retries every 5 minutes, forever) cannot push the
// panic out of the report.
func TestReportLogsBudgets(t *testing.T) {
	noisy := strings.Repeat("[funnel] could not start CloudFlare tunnel - retrying in 5m0s\n", 4000)
	if len(noisy) <= daemonLogReportBytes {
		t.Fatalf("test fixture is not over budget (%d bytes)", len(noisy))
	}
	stubUnarr(t, map[string]reply{
		"daemon logs":        {out: noisy + "LAST-DAEMON-LINE\n"},
		"daemon logs --boot": {out: "panic: boom\n"},
	})

	body := collectReportLogs()
	if len(body) > maxReportLogBytes {
		t.Fatalf("assembled body is %d bytes, over the %d report cap", len(body), maxReportLogBytes)
	}
	if !bytes.Contains(body, []byte("panic: boom")) {
		t.Fatal("a noisy daemon log must not crowd out the panic")
	}
	if !bytes.Contains(body, []byte("LAST-DAEMON-LINE")) {
		t.Fatal("the daemon section must keep its TAIL — the newest lines are the interesting ones")
	}
	if !bytes.Contains(body, []byte("trimmed to the last")) {
		t.Fatal("a trimmed section must say so, or it reads as a complete file starting mid-sentence")
	}
}

// TestReportLogsBothSectionsOverBudget: the boot log has its own cap, so a
// runaway startup log cannot eat the whole report either.
func TestReportLogsBothSectionsOverBudget(t *testing.T) {
	stubUnarr(t, map[string]reply{
		"daemon logs":        {out: strings.Repeat("d\n", daemonLogReportBytes)},
		"daemon logs --boot": {out: strings.Repeat("b\n", bootLogReportBytes) + "panic: last\n"},
	})

	body := collectReportLogs()
	if len(body) > maxReportLogBytes {
		t.Fatalf("assembled body is %d bytes, over the %d report cap", len(body), maxReportLogBytes)
	}
	if !bytes.Contains(body, []byte("panic: last")) {
		t.Fatal("the tail of the startup log is what a crash report is for")
	}
	if !bytes.Contains(body, []byte("daemon log")) {
		t.Fatal("an over-budget startup log must not evict the daemon section entirely")
	}
}

// TestRenderedFramingIsASCII: the assembled text is not only posted as JSON —
// dumpLogs writes it to a .txt the user opens on Windows, where a BOM-less
// UTF-8 file decoded as CP1252 mangles every non-ASCII byte. The log lines
// themselves already suffer from that; the framing must not add to it.
func TestRenderedFramingIsASCII(t *testing.T) {
	stubUnarr(t, map[string]reply{
		"daemon logs":        {out: strings.Repeat("x\n", daemonLogReportBytes) + "tail\n"},
		"daemon logs --boot": {out: "boot\n"},
	})

	for _, body := range [][]byte{collectReportLogs(), collectLogs()} {
		for i, b := range body {
			if b > 0x7F {
				t.Fatalf("non-ASCII byte %#x at offset %d in the framing: %q",
					b, i, string(body[max(0, i-40):min(len(body), i+40)]))
			}
		}
	}
}

// TestCollectLogsIsWholeForTheUser: "View logs" and the mail fallback write to
// the user's own disk, where a budget would only hide their own data — and both
// sources must be there, not just the daemon log.
func TestCollectLogsIsWholeForTheUser(t *testing.T) {
	big := strings.Repeat("x\n", daemonLogReportBytes)
	stubUnarr(t, map[string]reply{
		"daemon logs":        {out: big},
		"daemon logs --boot": {out: "panic: in the dump too\n"},
	})

	body := collectLogs()
	if len(body) < len(big) {
		t.Fatalf("collectLogs trimmed the user's own log: %d bytes from %d", len(body), len(big))
	}
	if !bytes.Contains(body, []byte("panic: in the dump too")) {
		t.Fatal("View logs must show the startup log too: it is where a user looking for a crash has to look")
	}
	if bytes.Contains(body, []byte("trimmed to the last")) {
		t.Fatal("the on-disk dump must not be trimmed")
	}
}

// TestRenderSeparatesSectionsWithoutTrailingNewlines: a log that does not end
// in a newline (a truncated file, a CLI that prints without one) must not run
// its last line into the next section's header.
func TestRenderSeparatesSectionsWithoutTrailingNewlines(t *testing.T) {
	stubUnarr(t, map[string]reply{
		"daemon logs":        {out: "daemon-last-line-no-newline"},
		"daemon logs --boot": {out: "boot-last-line-no-newline"},
	})

	body := string(collectReportLogs())
	if strings.Contains(body, "daemon-last-line-no-newline=====") {
		t.Fatalf("a section without a trailing newline ran into the next header:\n%s", body)
	}
	if !strings.Contains(body, "daemon-last-line-no-newline\n") {
		t.Fatalf("the missing trailing newline was not supplied:\n%s", body)
	}
	if !strings.HasSuffix(body, "boot-last-line-no-newline\n\n") {
		t.Fatalf("the final section must end newline-terminated:\n%q", body[max(0, len(body)-60):])
	}
}

// TestRenderNoSections covers the defensive branch no caller can reach today
// (logSections always returns the daemon section, placeholder or not). Pinned
// so a future third source, or an early return, cannot turn "nothing to say"
// into a panic on a nil slice.
func TestRenderNoSections(t *testing.T) {
	if got := renderLogSections(nil, true); got != nil {
		t.Fatalf("renderLogSections(nil) = %q, want nil", got)
	}
	if got := renderLogSections([]logSection{}, false); got != nil {
		t.Fatalf("renderLogSections(empty) = %q, want nil", got)
	}
}

// TestTailLinesNeverStartsMidLine: a byte-exact tail leaves a partial first
// line, which reads as corruption in a report.
func TestTailLinesNeverStartsMidLine(t *testing.T) {
	in := []byte("first line\nsecond line\nthird line\n")
	got := tailLines(in, 20)
	if bytes.HasPrefix(got, []byte("d line")) || !bytes.HasPrefix(got, []byte("second")) && !bytes.HasPrefix(got, []byte("third")) {
		t.Fatalf("tailLines(%q, 20) = %q, want a whole-line suffix", in, got)
	}
	if whole := tailLines(in, 1000); !bytes.Equal(whole, in) {
		t.Fatalf("tailLines must pass a slice that fits through untouched, got %q", whole)
	}
	// A single line longer than the budget has no newline to cut at: the tail is
	// better than nothing, and must not come back empty.
	if got := tailLines([]byte(strings.Repeat("y", 100)), 10); len(got) != 10 {
		t.Fatalf("a budget-busting single line must still yield its tail, got %d bytes", len(got))
	}
	// A tail whose cut lands exactly on a newline keeps the whole next line.
	if got := tailLines([]byte("aaaa\nbbbb\n"), 5); string(got) != "bbbb\n" {
		t.Fatalf("tailLines cut on a newline boundary = %q, want %q", got, "bbbb\n")
	}
}
