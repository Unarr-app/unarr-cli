// Command unarr-desktop is a minimal system-tray companion for the unarr agent.
//
// It is a SEPARATE binary from the headless `unarr` daemon on purpose: the tray
// uses fyne.io/systray, which on Linux speaks DBus/StatusNotifierItem (pure Go,
// CGO_ENABLED=0 — no GTK/AppIndicator dev libs). goreleaser builds only
// ./cmd/unarr, so this package never enters the daemon's signed cross-compile
// pipeline; the desktop app gets its own per-OS build (see docs: Vía B).
//
// Scope: tray icon + menu — agent status, pause/resume/restart, open the web
// app, configure the agent on the web (paths/codecs/hardware — the same data
// the web shows), edit config.toml, view logs, send logs to support, and crash
// control: if the daemon dies unexpectedly a report is generated and sent to
// the developers (support@unarr.app). The rich UI is the web app opened in the
// browser; there is no native window to build or maintain.
package main

import (
	_ "embed"
	"fmt"
	"os"
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

func main() {
	sentry.Init(version)
	defer sentry.Close()
	defer sentry.RecoverPanic()
	systray.Run(onReady, func() {})
}

func onReady() {
	icons := buildStateIcons(trayIcon)
	systray.SetIcon(trayIcon)
	systray.SetTitle("unarr")
	systray.SetTooltip("unarr agent")

	// applyState swaps the tray icon + tooltip only on transitions (SetIcon on
	// every 5s tick would spam DBus/Cocoa for nothing).
	shown := stateUnknown
	applyState := func(st trayState) {
		if st == shown {
			return
		}
		shown = st
		systray.SetIcon(icons[st])
		systray.SetTooltip("unarr agent — " + st.label())
	}

	mStatus := systray.AddMenuItem("Checking…", "Agent status")
	mStatus.Disable()
	systray.AddSeparator()
	mPause := systray.AddMenuItem("Pause agent", "Stop the agent (downloads and streams halt)")
	mResume := systray.AddMenuItem("Resume agent", "Start the agent")
	mRestart := systray.AddMenuItem("Restart agent", "Restart the agent")
	systray.AddSeparator()
	mOpen := systray.AddMenuItem("Open unarr.app", "Open the unarr web app")
	mConfigure := systray.AddMenuItem("Configure agent (web)", "Paths, codecs, hardware — on the web")
	mEdit := systray.AddMenuItem("Edit config.toml", "Open the agent config file")
	systray.AddSeparator()
	mLogs := systray.AddMenuItem("View logs", "Open the agent log file")
	mSendLogs := systray.AddMenuItem("Send logs to support", "Send agent logs to the developers")
	mDocs := systray.AddMenuItem("Documentation", "Open the unarr docs")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Close the tray (the agent keeps running)")

	// Crash watcher state — touched only from refresh()/control() call sites
	// below; systray delivers those on independent goroutines but never
	// concurrently in practice (ticker + click loop), and the worst race is a
	// duplicate notification, so plain fields are fine.
	var suppressCrashUntil time.Time
	var reportedCrashPID int

	// refresh reflects daemon state into the status row + pause/resume/restart
	// enablement. Read from the same state file `unarr status` uses (no drift).
	refresh := func() {
		s := readStatus()
		if s.running && isPausedMarker() {
			markPaused(false) // resumed outside the tray (CLI/web) — self-heal
		}
		st := displayState(s, isPausedMarker())
		applyState(st)
		switch st {
		case stateRunning:
			title := fmt.Sprintf("Agent: running (PID %d)", s.pid)
			if s.tasks > 0 {
				title += fmt.Sprintf(" · %d task(s)", s.tasks)
			}
			mStatus.SetTitle(title)
			mPause.Enable()
			mResume.Disable()
			mRestart.Enable()
		case stateCrashed:
			mStatus.SetTitle("Agent: crashed")
			mPause.Disable()
			mResume.Enable()
			mRestart.Disable()
			if s.pid != reportedCrashPID && time.Now().After(suppressCrashUntil) {
				reportedCrashPID = s.pid
				go handleCrash(s)
			}
		case statePaused:
			mStatus.SetTitle("Agent: paused")
			mPause.Disable()
			mResume.Enable()
			mRestart.Disable()
		default:
			mStatus.SetTitle("Agent: stopped")
			mPause.Disable()
			mResume.Enable()
			mRestart.Disable()
		}
	}
	refresh()

	// control execs a daemon command, surfaces spawn errors, and nudges a refresh
	// shortly after (the state file updates asynchronously) on top of the ticker.
	control := func(args ...string) {
		suppressCrashUntil = time.Now().Add(crashSuppressWindow)
		if err := runUnarr(args...); err != nil {
			fmt.Fprintln(os.Stderr, "unarr-desktop: control:", err)
		}
		time.AfterFunc(1500*time.Millisecond, refresh)
	}

	go func() {
		t := time.NewTicker(statusPeriod)
		defer t.Stop()
		for range t.C {
			refresh()
		}
	}()

	// onReady MUST return so the Linux DBus backend exports the menu — handle
	// clicks in a goroutine, never block here.
	go func() {
		for {
			select {
			case <-mPause.ClickedCh:
				markPaused(true)
				control("stop")
			case <-mResume.ClickedCh:
				markPaused(false)
				control("start")
			case <-mRestart.ClickedCh:
				markPaused(false)
				control("daemon", "restart")
			case <-mOpen.ClickedCh:
				openURL(webBase())
			case <-mConfigure.ClickedCh:
				openURL(hubURL())
			case <-mEdit.ClickedCh:
				openFile(configPath())
			case <-mLogs.ClickedCh:
				openLogs()
			case <-mSendLogs.ClickedCh:
				go sendLogsToSupport()
			case <-mDocs.ClickedCh:
				openURL(docsURL())
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
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
