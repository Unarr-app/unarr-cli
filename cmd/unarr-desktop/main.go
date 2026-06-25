// Command unarr-desktop is a minimal system-tray companion for the unarr agent.
//
// It is a SEPARATE binary from the headless `unarr` daemon on purpose: the tray
// needs CGO (fyne.io/systray binds GTK/AppIndicator on Linux, Cocoa on macOS,
// Win32 on Windows), whereas the daemon ships with CGO_ENABLED=0 so it
// cross-compiles cleanly and signs in a single pass. goreleaser builds only
// ./cmd/unarr, so this package never enters that pipeline — the desktop app gets
// its own per-OS build (see docs: Vía B).
//
// Scope (spike): tray icon + menu (status / open unarr / quit). The rich UI is
// the existing web app, opened in the default browser — no native window to
// build or maintain.
package main

import (
	_ "embed"
	"fmt"
	"net"
	"os"
	"time"

	"fyne.io/systray"
	"github.com/pkg/browser"
)

//go:embed icon.png
var trayIcon []byte

const (
	// streamAddr is where the running agent binds its local stream server
	// (downloads.stream_port default). Used as a cheap liveness probe.
	streamAddr   = "127.0.0.1:11818"
	statusPeriod = 5 * time.Second
)

// webURL is where "Open unarr" points. Override with UNARR_API_URL (the same var
// the agent already honors); defaults to the public app.
func webURL() string {
	if v := os.Getenv("UNARR_API_URL"); v != "" {
		return v
	}
	return "https://unarr.app"
}

// daemonRunning reports whether the local agent's stream server is listening —
// a cheap proxy for "the agent is up" without coupling to the daemon's internals.
func daemonRunning() bool {
	conn, err := net.DialTimeout("tcp", streamAddr, 400*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
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
	mOpen := systray.AddMenuItem("Open unarr", "Open the unarr web app")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Close the tray (the agent keeps running)")

	// Reflect daemon liveness in the (disabled) status row.
	go func() {
		for {
			if daemonRunning() {
				mStatus.SetTitle("Agent: running")
			} else {
				mStatus.SetTitle("Agent: stopped")
			}
			time.Sleep(statusPeriod)
		}
	}()

	// Handle clicks in a goroutine — onReady MUST return so the systray backend
	// can finish exporting the menu. On the Linux DBus/StatusNotifierItem backend
	// a blocking onReady leaves the com.canonical.dbusmenu unexported, which the
	// host renders as an EMPTY menu (icon shows, but no items).
	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				if err := browser.OpenURL(webURL()); err != nil {
					fmt.Fprintln(os.Stderr, "unarr-desktop: open url:", err)
				}
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}
