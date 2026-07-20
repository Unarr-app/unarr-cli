//go:build darwin

package main

// installTapHandler is a no-op on macOS: the menu bar item keeps its native
// gesture, where clicking the icon opens the menu.
//
// Registering a primary-tap handler here would be actively wrong. systray's
// macOS backend installs one NSEvent monitor for
// NSEventTypeLeftMouseDown|NSEventTypeRightMouseDown and unconditionally calls
// leftMouseClicked from it, swallowing the event — so a right-click would fire
// the primary handler too, opening the browser every time the user reached for
// the menu, and the menu itself would only be left to a rightMouseUp: whose
// matching down was already consumed. Leaving the handler unset keeps the
// backend's show_menu() fallback on both buttons, which is also the convention
// macOS users expect from a menu bar extra.
func installTapHandler() {}
