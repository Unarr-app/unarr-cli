//go:build darwin

package main

import "testing"

func TestInstallTapHandlerRegistersNothingOnMacOS(t *testing.T) {
	// This is a regression guard, not a formality. systray's macOS backend
	// installs a single NSEvent monitor for
	// NSEventTypeLeftMouseDown|NSEventTypeRightMouseDown and unconditionally
	// calls leftMouseClicked from it, swallowing the event. So registering a
	// primary-tap handler here would also fire it on right-click — opening the
	// browser every time the user reached for the menu — and would leave the
	// menu to a rightMouseUp: whose matching down was already consumed.
	//
	// Leaving it unregistered keeps the backend's show_menu() fallback on both
	// buttons, which is the gesture macOS users expect from a menu bar extra.
	registered := false
	prev := setOnTapped
	setOnTapped = func(func()) { registered = true }
	t.Cleanup(func() { setOnTapped = prev })

	installTapHandler()

	if registered {
		t.Fatal("registered a primary-tap handler on macOS: right-click would open the browser instead of the menu")
	}
}
