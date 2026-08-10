package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/pkg/browser"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/notify"
)

const (
	supportEmail = "support@unarr.app"
	// maxReportLogBytes bounds the log tail included in a report. The server
	// caps the body anyway; 64 KiB of tail is plenty to diagnose a crash.
	//
	// collectReportLogs has already trimmed each source to its own budget, so
	// this is a backstop for the headers and for a future third section — not
	// the cut that decides what a developer gets to read.
	maxReportLogBytes = 64 << 10
	reportTimeout     = 30 * time.Second
)

// sendReport gathers agent metadata + a bounded log tail and posts it to the
// server, which emails it to the developers. Fails with a descriptive error
// when the agent has no API credentials yet (never registered / logged out).
//
// `about` is the agent this report DESCRIBES, captured by the caller at the
// moment it decided to report. Passing it in is the whole point: this function
// used to re-derive the same facts by reading the daemon state file again —
// resolveAgentVersion() read it once, agent.ReadState() read it a second time —
// which made a single report the product of THREE reads of a file that is being
// concurrently removed.
//
// That is not theoretical. A crashed daemon's state file is deliberately left on
// disk, but it does not stay: the tray reaps it, and a restart replaces it. When
// it vanished between the reads, the version fell through to a cached probe of
// the INSTALLED binary while the message kept the dead agent's version, and the
// id came back empty — which the server renders as "unregistered". A field
// report arrived headed "Agent: unregistered v1.0.4-beta" whose own message said
// "PID 11808, v1.9.0": one report, two agents, no id.
//
// A zero `about` (the user-initiated "Send logs to support" path, where no
// specific agent is under discussion) still falls back to reading the state
// file — there is nothing better to use, and no crash is being described.
func sendReport(kind, message string, about agentStatus) error {
	client, _, err := newAgentClient()
	if err != nil {
		return err
	}

	agentVersion, agentID := about.version, about.agentID
	if agentVersion == "" {
		agentVersion = resolveAgentVersion()
	}
	if agentID == "" {
		if st := agent.ReadState(); st != nil {
			agentID = st.AgentID
		}
	}

	report := agent.SupportReport{
		Kind:           kind,
		Message:        message,
		Logs:           string(tailBytes(collectReportLogs(), maxReportLogBytes)),
		AgentVersion:   agentVersion,
		DesktopVersion: version,
		AgentID:        agentID,
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
	}

	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	return client.SendSupportReport(ctx, report)
}

// agentVersionBestEffort asks the installed daemon binary for its version; the
// state file (when present) overrides this with the running daemon's version.
func agentVersionBestEffort() string {
	out, err := runUnarrOutput("version")
	if err != nil || len(out) == 0 {
		return "unknown"
	}
	return parseUnarrVersion(firstLine(string(out)))
}

// parseUnarrVersion extracts the bare version token from an `unarr version`
// banner line ("unarr 1.5.2 (linux/amd64)" → "1.5.2"); menu rows and reports
// want the number, not the banner. Unknown shapes pass through untouched.
func parseUnarrVersion(line string) string {
	fields := strings.Fields(line)
	if len(fields) >= 2 && fields[0] == "unarr" {
		return fields[1]
	}
	return line
}

// mailFallback: the API path failed (no key, server unreachable). Dump the logs
// to disk, open them, and open a prefilled mail draft to support@unarr.app so
// the user can attach the dump by hand.
func mailFallback(cause error) {
	dump, dumpErr := dumpLogs()
	body := "Hi,\n\nI want to send the unarr agent logs to support.\n\n" +
		"Automatic submission failed with: " + cause.Error() + "\n"
	if dumpErr == nil {
		body += "\nThe log file was saved at:\n  " + dump +
			"\n\nPlease attach that file to this email before sending.\n"
	}
	subject := fmt.Sprintf("unarr agent logs (desktop v%s, %s/%s)", version, runtime.GOOS, runtime.GOARCH)
	mailto := "mailto:" + supportEmail +
		"?subject=" + url.QueryEscape(subject) +
		"&body=" + url.QueryEscape(body)
	if err := browser.OpenURL(mailto); err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: mail fallback:", err)
	}
	if dumpErr == nil {
		if err := openPath(dump); err != nil {
			fmt.Fprintln(os.Stderr, "unarr-desktop: open dump:", err)
		}
	}
	notify.Send("Could not send automatically",
		"A mail draft to "+supportEmail+" was opened — please attach the log file shown.")
}

// tailBytes returns the last max bytes of b (whole slice when smaller).
func tailBytes(b []byte, max int) []byte {
	if len(b) <= max {
		return b
	}
	return b[len(b)-max:]
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			return s[:i]
		}
	}
	return s
}
