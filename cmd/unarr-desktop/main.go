// Command unarr-desktop is a minimal system-tray companion for the unarr agent.
//
// It is a SEPARATE binary from the headless `unarr` daemon on purpose: the tray
// uses fyne.io/systray, which on Linux speaks DBus/StatusNotifierItem (pure Go,
// CGO_ENABLED=0 — no GTK/AppIndicator dev libs). goreleaser builds only
// ./cmd/unarr, so this package never enters the daemon's signed cross-compile
// pipeline; the desktop app gets its own per-OS build (see docs: Vía B).
//
// Scope: tray icon + menu — agent status, pause/resume/restart, account/plan
// rows with an upgrade CTA, open the web app, configure the agent on the web,
// edit config.toml, view logs, send logs to support, "Start at login", and
// crash control: if the daemon dies unexpectedly a report is generated and
// sent to the developers (support@unarr.app). The rich UI is the web app
// opened in the browser; there is no native window to build or maintain.
package main

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
	"time"

	"fyne.io/systray"
	"github.com/pkg/browser"

	"github.com/Unarr-app/unarr-cli/internal/notify"
	"github.com/Unarr-app/unarr-cli/internal/sentry"
)

//go:embed icon.png
var trayIcon []byte

// version is stamped at build time via
// -ldflags "-X main.version=x.y.z" (CI); "dev" for local builds.
var version = "dev"

const statusPeriod = 5 * time.Second

// accountPeriod: the account row only changes on plan/sign-in events, so a slow
// refresh is enough and keeps the tray from hammering the API. Out-of-band
// kicks (CTA click, daemon start transition) cover the moments that matter.
const accountPeriod = 30 * time.Minute

// crashSuppressWindow silences crash detection right after a tray-initiated
// stop/restart: the daemon removes its state file asynchronously, so for a few
// seconds a stale state file + dead PID would look exactly like a crash.
const crashSuppressWindow = 20 * time.Second

// webBase is where "Open unarr.app" points. Override with UNARR_API_URL (the same
// var the agent already honors); defaults to the public app.
func webBase() string {
	if v := os.Getenv("UNARR_API_URL"); v != "" {
		return v
	}
	return "https://unarr.app"
}

// hubURL is the in-app agents hub: status, paths, codecs, hardware + config — the
// authoritative view, so "Configure agent" can never drift from what the web shows.
func hubURL() string  { return webBase() + "/profile?tab=agents" }
func docsURL() string { return webBase() + "/docs" }

func openURL(url string) {
	if err := browser.OpenURL(url); err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: open url:", err)
	}
}

func openFile(path string) {
	if path == "" {
		fmt.Fprintln(os.Stderr, "unarr-desktop: no path to open")
		return
	}
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: not found:", path)
		return
	}
	if err := openPath(path); err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: open file:", err)
	}
}

// openLogs captures the daemon's logs to a temp file and opens it in a viewer.
func openLogs() {
	path, err := dumpLogs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: logs:", err)
		return
	}
	if err := openPath(path); err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: open logs:", err)
	}
}

// runMode classifies argv into exactly one action. The dispatch is STRICT on
// purpose: anything unrecognized used to fall through to systray.Run, so
// `--help`, a typo (`--updat`), or `--version extra` silently spawned a
// phantom tray (or hung a headless session waiting on DBus). Only a BARE
// invocation may ever start the tray.
type runMode int

const (
	modeTray runMode = iota
	modeVersion
	modeUpdate
	modeOpen
	modeHelp
	modeUsageError
)

const usageText = `unarr-desktop — system-tray companion for the unarr agent

Usage:
  unarr-desktop                            start the tray (no arguments)
  unarr-desktop --open <unarr://play?...>  hand the stream link to a local player
  unarr-desktop --update                   self-update this binary to the latest release
  unarr-desktop --version                  print the version
  unarr-desktop --help                     show this help
`

// dispatchArgs maps argv (without argv[0]) to a runMode; for modeOpen it also
// returns the raw unarr:// link. Pure — extracted from main so the strict
// fallthrough behavior is unit-testable.
func dispatchArgs(args []string) (runMode, string) {
	if len(args) == 0 {
		return modeTray, ""
	}
	// Protocol-handler forms first (`--open <url>`, `--open=<url>`, or a bare
	// unarr:// argument via %u/%1). A bare `--open` with no URL still maps to
	// modeOpen: runOpen("") prints usage and exits 2 without starting a tray.
	if raw, ok := openArg(args); ok {
		return modeOpen, raw
	}
	if len(args) == 1 {
		switch args[0] {
		case "--version":
			return modeVersion, ""
		case "--update":
			return modeUpdate, ""
		case "--help", "-h":
			return modeHelp, ""
		}
	}
	return modeUsageError, ""
}

func main() {
	mode, raw := dispatchArgs(os.Args[1:])
	switch mode {
	case modeVersion:
		// --version: also the self-updater's smoke test for a freshly
		// downloaded desktop binary (a bare invocation would start a tray —
		// never safe to exec blindly, hence a flag that can only print+exit).
		fmt.Println("unarr-desktop v" + version)
		return
	case modeUpdate:
		// --update: self-update for player-only installs (no `unarr` CLI).
		os.Exit(runDesktopSelfUpdate())
	case modeOpen:
		// Protocol-handler mode: parse, hand the stream to a local player,
		// exit. Ephemeral by design — no systray, no single-instance, no
		// sentry init (a browser click should launch the player in
		// milliseconds, and must never spawn a second tray).
		os.Exit(runOpen(raw))
	case modeHelp:
		fmt.Print(usageText)
		return
	case modeUsageError:
		fmt.Fprintf(os.Stderr, "unarr-desktop: unrecognized arguments: %s\n\n", strings.Join(os.Args[1:], " "))
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	case modeTray:
		// Default form (no args): fall through to the systray startup below.
		// Every other mode returns/exits above, so this is the only path that
		// reaches sentry.Init + systray.Run.
	}
	sentry.Init(version)
	defer sentry.Close()
	defer sentry.RecoverPanic()
	// Claim the unarr:// scheme for this executable before the tray starts
	// (Windows-only, HKCU, idempotent; Linux/macOS registration belongs to
	// install.sh / the future .app bundle — see register_*.go). Doing it on
	// every start self-heals the registration when the binary moves.
	registerURLScheme()
	systray.Run(onReady, func() {})
}

// onReady MUST return quickly: the Linux DBus backend exports the menu only
// after it does — every loop runs on its own goroutine.
func onReady() {
	ui := newTrayUI()
	ui.refresh()
	go ui.statusLoop()
	go ui.accountLoop()
	go ui.updateLoop()
	go ui.clickLoop()
}

// toggleAutostart flips "Start at login" to the opposite of the checkbox
// state. The checkbox only changes after the per-OS backend succeeded, so the
// menu never claims a state the OS artifact doesn't have; failures go to
// stderr AND a desktop notification (the menu is closed by the time the
// backend fails, so stderr alone would be invisible).
func toggleAutostart(item *systray.MenuItem) {
	enable := !item.Checked()
	if err := setAutostart(enable); err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: autostart:", err)
		verb := "enable"
		if !enable {
			verb = "disable"
		}
		notify.Send("Start at login failed",
			"Could not "+verb+" start at login: "+err.Error())
		return
	}
	if enable {
		item.Check()
	} else {
		item.Uncheck()
	}
}

// handleCrash runs when the daemon died without a clean shutdown (state file
// still on disk, status "running", PID gone). It notifies the user and sends a
// crash report — logs + agent metadata — to the developers, unless telemetry
// is disabled (UNARR_NO_TELEMETRY=1), in which case it only notifies.
func handleCrash(s agentStatus) {
	if os.Getenv("UNARR_NO_TELEMETRY") == "1" {
		notify.Send("unarr agent stopped unexpectedly",
			"Telemetry is disabled — use “Send logs to support” if you want to report it.")
		return
	}
	notify.Send("unarr agent stopped unexpectedly", "Collecting a crash report…")
	msg := fmt.Sprintf("Agent process (PID %d, v%s) died without a clean shutdown; detected by unarr-desktop.", s.pid, s.version)
	if err := sendReport("crash", msg); err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: crash report:", err)
		sentry.CaptureError(err, "desktop:crash-report")
		notify.Send("Crash report not sent",
			"Could not reach unarr.app — use “Send logs to support” later or email support@unarr.app.")
		return
	}
	notify.Send("Crash report sent", "The developers have been notified. You can resume the agent from the tray.")
}

// sendLogsToSupport is the user-initiated path ("Send logs to support"). It is
// always allowed (explicit user action, so UNARR_NO_TELEMETRY does not apply).
// When the agent has no API credentials yet, fall back to a mail draft +
// on-disk log dump the user can attach by hand.
func sendLogsToSupport() {
	notify.Send("Sending logs to support…", "Collecting agent logs.")
	err := sendReport("logs", "User-initiated log submission from the desktop tray.")
	if err == nil {
		notify.Send("Logs sent", "Thanks — the developers received your logs (support@unarr.app).")
		return
	}
	fmt.Fprintln(os.Stderr, "unarr-desktop: send logs:", err)
	sentry.CaptureError(err, "desktop:send-logs")
	mailFallback(err)
}
