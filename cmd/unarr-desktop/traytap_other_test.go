//go:build !darwin

package main

import "testing"

// captureRegistration swaps the registration seam so a test can see what
// installTapHandler wires up without a running tray.
func captureRegistration(t *testing.T) (handler *func(), calls *int) {
	t.Helper()
	var got func()
	var n int
	prev := setOnTapped
	setOnTapped = func(f func()) {
		n++
		got = f
	}
	t.Cleanup(func() { setOnTapped = prev })
	return &got, &n
}

func TestInstallTapHandlerRegistersThePrimaryTap(t *testing.T) {
	// Off macOS the left button must be wired: it is what makes the button do
	// anything at all on Cinnamon/XApp, whose SNI host ignores ItemIsMenu and
	// routes the left button to Activate regardless.
	t.Setenv("UNARR_API_URL", "")
	handler, calls := captureRegistration(t)

	installTapHandler()

	if *calls != 1 {
		t.Fatalf("registered the primary tap %d times, want exactly 1", *calls)
	}
	if *handler == nil {
		t.Fatal("registered a nil primary-tap handler; systray would keep ItemIsMenu=true")
	}
}

func TestRegisteredHandlerOpensTheWebApp(t *testing.T) {
	// Guards the wiring end to end: whatever was registered must be the action
	// that opens the web app, not some other func that merely satisfies a
	// non-nil check.
	t.Setenv("UNARR_API_URL", "")
	handler, _ := captureRegistration(t)
	opened := captureTaps(t)

	installTapHandler()
	if *handler == nil {
		t.Fatal("nothing was registered")
	}
	(*handler)()

	if got, want := waitForTap(t, opened), "https://unarr.app"; got != want {
		t.Errorf("the registered handler opened %q, want %q", got, want)
	}
}
