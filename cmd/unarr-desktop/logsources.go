package main

// Where the tray gets "the logs" from — for "View logs", for the mail fallback,
// and for the body of a support/crash report.
//
// THERE ARE TWO LOGS, AND THE CRASH IS ONLY EVER IN THE SECOND ONE.
//
// A supervised daemon writes through log.SetOutput into a file it owns
// (unarr.log). A Go panic does not go through log.SetOutput — the runtime dumps
// it straight to stderr, which the service launcher redirects into a SEPARATE
// file (unarr.boot.log), along with the start banner and any fatal error from a
// start that never got going. See internal/cmd/daemon_launch_vbs.go.
//
// The report only ever carried the first file. So every crash report ever sent
// was, by construction, incapable of containing the crash: a field report of a
// daemon that vanished mid-run arrived with a perfectly healthy log tail and no
// panic anywhere, because the panic was sitting in a file nobody collected.
// Both files go in now.
//
// READING THE SECOND FILE NEEDS `unarr logs --boot`, which arrives with the
// daemon log-ownership work and is not on every branch. Where the flag is
// missing the CLI answers "unknown flag", bootLogSection reports no section,
// and a report is byte for byte what it was before — the tray degrades to the
// old single-log body instead of breaking. So this can ship ahead of the flag;
// it simply starts carrying panics the day the flag lands.

import (
	"bytes"
	"fmt"
)

const (
	// Per-section caps for a report body. They sum to just under
	// maxReportLogBytes so the headers still fit and the final tail in
	// sendReport is a backstop, not the thing doing the trimming.
	daemonLogReportBytes = 44 << 10
	bootLogReportBytes   = 16 << 10
)

// logSection is one named source of log text, with the budget it gets when it
// goes into a report. Zero budget means "no cap" (the on-disk dump).
type logSection struct {
	title  string
	body   []byte
	budget int
}

// collectLogs returns every log source, whole, for "View logs" and the mail
// fallback — a user reading their own logs on their own disk needs no budget.
func collectLogs() []byte { return renderLogSections(logSections(), false) }

// collectReportLogs returns the same sources trimmed to their per-section
// budgets, for the body of a support or crash report.
//
// The startup log goes LAST, and that ordering is load-bearing: sendReport
// tails the whole body one final time, so whatever sits at the end is what
// survives a body that is still too big. The panic is worth more than another
// 16 KiB of DHT bookkeeping.
func collectReportLogs() []byte { return renderLogSections(logSections(), true) }

// logSections reads each source through the CLI, which knows where its own logs
// live (journald for a systemd service, files otherwise) so the tray never has
// to guess a path.
//
// The two reads run CONCURRENTLY, and that is not premature: each runUnarrOutput
// is bounded at 30s (a binary on a hung network mount, an AV scanner holding the
// exe), and "View logs" is dispatched straight from the menu's click loop — no
// goroutine of its own. Sequential reads would have doubled that worst case to a
// minute of a menu that answers no further clicks, as the price of a second log
// file. Concurrent, it stays where it was.
func logSections() []logSection {
	bootCh := make(chan logSection, 1)
	go func() {
		s, ok := bootLogSection()
		if !ok {
			close(bootCh)
			return
		}
		bootCh <- s
	}()

	out, err := runUnarrOutput("daemon", "logs")
	if len(out) == 0 {
		// The CLI could not answer. Before giving up on the daemon log, read the
		// file off disk: the one failure that lost a whole field report was a
		// binary that would not exec while its log files sat there readable.
		if body, ok := fallbackDaemonLog(); ok {
			out = fallbackNote(err, body)
		} else {
			out = []byte(noLogsExplanation(err))
		}
	}
	sections := []logSection{{
		title:  "daemon log (unarr logs)",
		body:   out,
		budget: daemonLogReportBytes,
	}}
	if boot, ok := <-bootCh; ok {
		sections = append(sections, boot)
	}
	return sections
}

// noLogsExplanation says why a log read came back with nothing — and it
// distinguishes the two reasons, because they point at opposite things.
//
// "The CLI ran and printed nothing" IS usually a foreground daemon whose output
// went to a terminal, and telling the user to install it as a service is the
// right advice.
//
// "The CLI could not be run at all" is not that, and giving the same advice for
// it is actively misleading. A field crash report came back with a log tail of
// nothing but `No logs available. (exit status 0xc0000142)` followed by the
// advice to install the agent as a service — on a box where the agent WAS
// installed as a service, and where 0xc0000142 (STATUS_DLL_INIT_FAILED) means
// the Windows loader killed the collector before main(). The report blamed the
// user's setup for a corrupt binary, and pointed whoever read it away from the
// actual fault.
//
// So a failed exec is reported as a failed exec, and the report says the
// collection failed rather than implying the daemon had nothing to say.
func noLogsExplanation(err error) string {
	if err != nil {
		return "COULD NOT READ THE LOGS: running `unarr daemon logs` failed (" +
			err.Error() + ").\nThis is a failure of the log collection itself, NOT" +
			" evidence about the agent - the agent's own logs, if any, are not in" +
			" this report.\nOn Windows, an 'exit status 0xc...' here means the binary" +
			" could not start at all (a corrupt or partially written unarr.exe, or an" +
			" antivirus holding it).\n"
	}
	return "No logs available.\nIf the agent runs in the foreground, logs go to its" +
		" terminal. Install it as a service (unarr daemon install) to persist them.\n"
}

// bootLogSection reads the launcher's own log, and is allowed to come back
// empty-handed WITHOUT saying so anywhere.
//
// Every reason it can fail is a normal state of the world, not a fault worth
// pasting into a report: a systemd install has no such file (its startup output
// is in the journal, already covered by the daemon section); a daemon that has
// only ever run in the foreground never created one; and a CLI older than this
// flag answers "unknown flag: --boot". Reporting any of those would put a
// confusing error in front of a developer reading a crash report, so a failure
// is simply an absent section.
func bootLogSection() (logSection, bool) {
	out, err := runUnarrOutput("daemon", "logs", "--boot")
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		// Same fallback as the daemon log, and it matters MORE here: this is the
		// file panic dumps land in, and it is lost to the identical exec failure
		// because both reads run the same binary. A missing file still yields no
		// section — only a real, non-empty boot log turns into one.
		body, ok := fallbackBootLog()
		if !ok {
			return logSection{}, false
		}
		out = fallbackNote(err, body)
	}
	return logSection{
		title:  "startup log (unarr logs --boot): panics and start failures land here",
		body:   out,
		budget: bootLogReportBytes,
	}, true
}

// renderLogSections concatenates the sections under headers. A single section
// is rendered bare, exactly as before, so the common "no boot log here" case
// (systemd, foreground) reads identically to what users and developers are
// used to.
//
// EVERY BYTE THIS FUNCTION ADDS IS ASCII, on purpose. The assembled text is not
// only posted as JSON: dumpLogs writes it to a .txt the user opens in whatever
// Windows hands them, and a BOM-less UTF-8 file read through CP1252 turns an em
// dash into three characters. The log lines themselves already suffer from that
// (it is visible in the field reports); the framing this adds around them must
// not add more of it. Same lesson as test/windows/smoke-boottime.ps1.
func renderLogSections(sections []logSection, bounded bool) []byte {
	if len(sections) == 0 {
		return nil
	}
	if len(sections) == 1 {
		return sectionBody(sections[0], bounded)
	}
	var buf bytes.Buffer
	for _, s := range sections {
		fmt.Fprintf(&buf, "===== %s =====\n", s.title)
		body := sectionBody(s, bounded)
		buf.Write(body)
		if n := len(body); n > 0 && body[n-1] != '\n' {
			buf.WriteByte('\n')
		}
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// sectionBody applies the section's budget when one is in force, and says so —
// a tail with no marker reads as a complete file that happens to start
// mid-sentence.
func sectionBody(s logSection, bounded bool) []byte {
	if !bounded || s.budget <= 0 || len(s.body) <= s.budget {
		return s.body
	}
	kept := tailLines(s.body, s.budget)
	return append([]byte(fmt.Sprintf("[... trimmed to the last %d bytes ...]\n", len(kept))), kept...)
}

// tailLines is tailBytes that never starts mid-line: the partial first line a
// byte cut leaves behind is dropped. Returns the whole slice when it fits.
func tailLines(b []byte, max int) []byte {
	if len(b) <= max {
		return b
	}
	cut := tailBytes(b, max)
	if i := bytes.IndexByte(cut, '\n'); i >= 0 && i+1 < len(cut) {
		return cut[i+1:]
	}
	return cut
}
