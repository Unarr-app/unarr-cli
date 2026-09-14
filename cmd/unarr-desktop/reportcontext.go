package main

// What a report says about the daemon it describes, before any log line.
//
// A field crash report (2026-09-14, windows, v1.11.6) carried a perfectly
// ordinary log tail — from eight days before the daemon that died. That daemon
// had been started without a log file, so unarr.log and unarr.boot.log simply
// stopped moving while it ran; the report gave no hint of it, and the tail read
// as evidence about a crash it knew nothing of.
//
// So every report now opens with the facts that settle whether its log sections
// can be trusted: which run it describes, whether that run claimed a log file,
// and when each file last changed, with an explicit STALE line when the daemon
// log is older than the daemon's own last sign of life. The host's boot and
// shutdown instants ride along, since "did the machine go down under it" is the
// next question every crash report raises.
//
// ASCII only, like the rest of the framing in logsources.go.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/sysinfo"
)

// staleLogThreshold is how much older than the daemon's START the daemon log may
// be before it is called someone else's log.
//
// Measured against the start, never against the last sign of life: an idle
// daemon can stay silent for hours while LastAlive ticks every few seconds (with
// the VPN kill-switch on there is not even DHT bookkeeping to log), so "older
// than last alive" would accuse a healthy log. A run that writes the file at all
// writes its startup banner, so a file last touched well BEFORE the run began
// cannot describe it. The margin covers the gap between the banner and the
// StartedAt stamp.
const staleLogThreshold = 10 * time.Minute

type reportContext struct {
	pid       int
	startedAt time.Time
	lastAlive time.Time
	logFile   string
}

// contextFor uses the agent the caller captured. A zero one (the user-initiated
// "Send logs" path, which describes no particular crash) falls back to the state
// file, the same fallback sendReport uses for the agent id.
func contextFor(about agentStatus) reportContext {
	if about.pid != 0 {
		return reportContext{
			pid:       about.pid,
			startedAt: about.startedAt,
			lastAlive: about.lastAlive,
			logFile:   about.logFile,
		}
	}
	st := agent.ReadState()
	if st == nil {
		return reportContext{}
	}
	return reportContext{
		pid:       st.PID,
		startedAt: st.StartedAt,
		lastAlive: agent.LastAliveAt(st),
		logFile:   st.LogFile,
	}
}

func renderReportContext(c reportContext) string {
	var b strings.Builder
	b.WriteString("===== report context =====\n")
	if c.pid == 0 {
		b.WriteString("daemon: no state file (not running, or never started)\n")
	} else {
		fmt.Fprintf(&b, "daemon: pid %d, started %s, last alive %s\n",
			c.pid, stamp(c.startedAt), stamp(c.lastAlive))
		b.WriteString(logClaimLine(c.logFile))
	}
	dir := config.DataDir()
	b.WriteString(daemonLogLine(filepath.Join(dir, fallbackDaemonLogName), c.startedAt))
	b.WriteString(bootLogLine(filepath.Join(dir, fallbackBootLogName)))
	boot, bootOK := sysinfo.BootTime()
	down, downOK := sysinfo.LastShutdown()
	fmt.Fprintf(&b, "host: booted %s, last recorded shutdown %s\n",
		stampIf(boot, bootOK), stampIf(down, downOK))
	b.WriteString("\n")
	return b.String()
}

func logClaimLine(logFile string) string {
	if logFile != "" {
		return "daemon log file: " + logFile + " (claimed by this run)\n"
	}
	return "daemon log file: none claimed - started in the foreground, by an older" +
		" tray's Resume, or by a launcher that redirects output itself; trust the" +
		" timestamps below, not the section headers\n"
}

func daemonLogLine(path string, startedAt time.Time) string {
	line, mod, ok := statLine(path)
	if !ok {
		return line
	}
	if !startedAt.IsZero() && startedAt.Sub(mod) > staleLogThreshold {
		line += fmt.Sprintf(" - STALE: last written %s before this daemon started;"+
			" this file does NOT describe this run",
			startedAt.Sub(mod).Round(time.Minute))
	}
	return line + "\n"
}

// bootLogLine reports the boot log without a staleness verdict: a healthy run
// writes nothing there after its banner, so an old file is the normal case.
func bootLogLine(path string) string {
	line, _, ok := statLine(path)
	if !ok {
		return line
	}
	return line + "\n"
}

// statLine describes a log file. ok is false when the file is not there, and
// the returned line is then already complete.
//
// The timestamp comes from an OPEN HANDLE, not from the path. A live daemon
// holds unarr.log open for its whole run, and NTFS may lag the directory
// entry's LastWriteTime behind a writer that keeps appending through an open
// handle. Seen ONCE on the Windows harness (a minute of new log lines while
// Get-Item kept the old time) and not reproduced on the next run, so treat it as
// possible, not certain. A path-based stat can read that lagging copy, and the
// STALE line below would then accuse a healthy log of belonging to someone else;
// the handle's own metadata is the safe side of that doubt.
func statLine(path string) (line string, mod time.Time, ok bool) {
	name := filepath.Base(path)
	f, err := os.Open(path)
	if err != nil {
		return name + ": not on disk\n", time.Time{}, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return name + ": unreadable (" + err.Error() + ")\n", time.Time{}, false
	}
	return fmt.Sprintf("%s: modified %s, %d bytes", name, stamp(fi.ModTime()), fi.Size()), fi.ModTime(), true
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

func stampIf(t time.Time, ok bool) string {
	if !ok {
		return "unknown"
	}
	return stamp(t)
}
