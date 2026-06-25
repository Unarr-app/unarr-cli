// Command unarr-desktop is a minimal system-tray companion for the unarr agent.
//
// It is a SEPARATE binary from the headless `unarr` daemon on purpose: the tray
// uses fyne.io/systray, which on Linux speaks DBus/StatusNotifierItem (pure Go,
// CGO_ENABLED=0 — no GTK/AppIndicator dev libs). goreleaser builds only
// ./cmd/unarr, so this package never enters the daemon's signed cross-compile
// pipeline; the desktop app gets its own per-OS build (see docs: Vía B).
//
// Scope: tray icon + menu — agent status, start/stop/restart, open the web app,
// manage the agent on the web (paths/codecs/hardware — the same data the web
// shows), edit config.toml, view logs, docs. The rich UI is the web app opened
// in the browser; there is no native window to build or maintain.
package main

import (
	_ "embed"
	"fmt"
	"os"
	"time"

	"fyne.io/systray"
	"github.com/pkg/browser"
)

//go:embed icon.png
var trayIcon []byte

const statusPeriod = 5 * time.Second

// webBase is where "Open unarr" points. Override with UNARR_API_URL (the same
// var the agent already honors); defaults to the public app.
func webBase() string {
	if v := os.Getenv("UNARR_API_URL"); v != "" {
		return v
	}
	return "https://unarr.app"
}

// hubURL is the in-app agents hub: status, paths, codecs, hardware + config — the
// authoritative view, so "Manage agent" can never drift from what the web shows.
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
	systray.Run(onReady, func() {})
}

func onReady() {
	systray.SetIcon(trayIcon)
	systray.SetTitle("unarr")
	systray.SetTooltip("unarr agent")

	mStatus := systray.AddMenuItem("Checking…", "Agent status")
	mStatus.Disable()
	systray.AddSeparator()
	mStart := systray.AddMenuItem("Start", "Start the agent")
	mStop := systray.AddMenuItem("Stop", "Stop the agent")
	mRestart := systray.AddMenuItem("Restart", "Restart the agent")
	systray.AddSeparator()
	mOpen := systray.AddMenuItem("Open unarr", "Open the unarr web app")
	mManage := systray.AddMenuItem("Manage agent (web)", "Status, paths, codecs, hardware — on the web")
	mEdit := systray.AddMenuItem("Edit config.toml", "Open the agent config file")
	mLogs := systray.AddMenuItem("View logs", "Open the agent log file")
	mDocs := systray.AddMenuItem("Documentation", "Open the unarr docs")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Close the tray (the agent keeps running)")

	// refresh reflects daemon state into the status row + start/stop/restart
	// enablement. Read from the same state file `unarr status` uses (no drift).
	refresh := func() {
		s := readStatus()
		if s.running {
			title := fmt.Sprintf("Agent: running (PID %d)", s.pid)
			if s.tasks > 0 {
				title += fmt.Sprintf(" · %d task(s)", s.tasks)
			}
			mStatus.SetTitle(title)
			mStart.Disable()
			mStop.Enable()
			mRestart.Enable()
		} else {
			mStatus.SetTitle("Agent: stopped")
			mStart.Enable()
			mStop.Disable()
			mRestart.Disable()
		}
	}
	refresh()

	// control execs a daemon command, surfaces spawn errors, and nudges a refresh
	// shortly after (the state file updates asynchronously) on top of the ticker.
	control := func(args ...string) {
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
			case <-mStart.ClickedCh:
				control("start")
			case <-mStop.ClickedCh:
				control("stop")
			case <-mRestart.ClickedCh:
				control("daemon", "restart")
			case <-mOpen.ClickedCh:
				openURL(webBase())
			case <-mManage.ClickedCh:
				openURL(hubURL())
			case <-mEdit.ClickedCh:
				openFile(configPath())
			case <-mLogs.ClickedCh:
				openLogs()
			case <-mDocs.ClickedCh:
				openURL(docsURL())
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}
