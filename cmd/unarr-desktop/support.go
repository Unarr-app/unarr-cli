package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"time"

	"github.com/pkg/browser"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/notify"
)

const (
	supportEmail = "support@unarr.app"
	// maxReportLogBytes bounds the log tail included in a report. The server
	// caps the body anyway; 64 KiB of tail is plenty to diagnose a crash.
	maxReportLogBytes = 64 << 10
	reportTimeout     = 30 * time.Second
)

// sendReport gathers agent metadata + a bounded log tail and posts it to the
// server, which emails it to the developers. Fails with a descriptive error
// when the agent has no API credentials yet (never registered / logged out).
func sendReport(kind, message string) error {
	cfg, err := config.Load(config.FilePath())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.ApplyEnvOverrides() // honor UNARR_API_URL / UNARR_API_KEY like the daemon does
	if cfg.Auth.APIKey == "" {
		return fmt.Errorf("agent has no API key (not logged in)")
	}
	base := cfg.Auth.APIURL
	if base == "" {
		base = webBase()
	}

	report := agent.SupportReport{
		Kind:           kind,
		Message:        message,
		Logs:           string(tailBytes(collectLogs(), maxReportLogBytes)),
		AgentVersion:   agentVersionBestEffort(),
		DesktopVersion: version,
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
	}
	if st := agent.ReadState(); st != nil {
		report.AgentID = st.AgentID
		if st.Version != "" {
			report.AgentVersion = st.Version
		}
	}

	client := agent.NewClient(base, cfg.Auth.APIKey, "unarr-desktop/"+version)
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
	return firstLine(string(out))
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
