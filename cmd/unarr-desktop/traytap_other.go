//go:build !darwin

package main

// installTapHandler gives the tray icon one uniform gesture outside macOS:
// left button opens the web app, right button opens the menu.
//
// Registering a primary-tap handler makes systray publish ItemIsMenu=false, so
// hosts that honour it (KDE/GNOME) stop treating the left button as "just open
// the menu" and call Activate here instead; the menu stays reachable because
// the Menu property is still exported and the right button uses it. On Windows
// the two buttons dispatch separately (WM_LBUTTONUP / WM_RBUTTONUP), so the
// right button still falls back to showing the menu.
//
// It also makes the left button work at all on Cinnamon/XApp, whose SNI host
// ignores ItemIsMenu and unconditionally routes the left button to Activate —
// with no handler registered that call failed with UnknownMethod, which is why
// the left button did nothing there.
func installTapHandler() {
	setOnTapped(trayTapped)
}
