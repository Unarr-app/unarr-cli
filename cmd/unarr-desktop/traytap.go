package main

import "fyne.io/systray"

// The primary-tap plumbing shared by every OS. Whether it is wired at all is a
// per-OS decision: traytap_other.go registers it, traytap_darwin.go does not.

// setOnTapped registers the primary-tap handler with the tray backend. It is a
// var so tests can observe registration without a running tray.
var setOnTapped = systray.SetOnTapped

// tapOpenURL opens a URL for a primary tap. It is a var so tests can observe
// taps without spawning a browser.
var tapOpenURL = openURL

// trayTapped is what a primary tap does: open the web app.
//
// It opens on its own goroutine because browser.OpenURL waits for the process
// it spawns while this runs inside the platform's click path — the Activate
// DBus method on Linux, the UI thread on Windows — which must not be held open
// (the same reason sendLogsToSupport is spawned).
func trayTapped() {
	go tapOpenURL(webBase())
}
