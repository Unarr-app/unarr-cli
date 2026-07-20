package main

import (
	"testing"
	"time"
)

// captureTaps swaps the tap opener for one that reports the URLs it was asked
// to open, restoring the real opener when the test ends. Tests must not run in
// parallel: the seam is a package var.
func captureTaps(t *testing.T) <-chan string {
	t.Helper()
	opened := make(chan string, 1)
	prev := tapOpenURL
	tapOpenURL = func(url string) { opened <- url }
	t.Cleanup(func() { tapOpenURL = prev })
	return opened
}

// waitForTap fails the test unless a tap opened a URL, and returns it. The tap
// runs on its own goroutine, so a channel with a deadline is the only sound way
// to read it.
func waitForTap(t *testing.T, opened <-chan string) string {
	t.Helper()
	select {
	case url := <-opened:
		return url
	case <-time.After(2 * time.Second):
		t.Fatal("a primary tap opened no URL")
		return ""
	}
}

func TestTrayTappedOpensTheWebApp(t *testing.T) {
	// The tap must land on the same target as the "Open unarr.app" menu item,
	// so the two cannot drift apart.
	t.Run("defaults to the public app", func(t *testing.T) {
		t.Setenv("UNARR_API_URL", "")
		opened := captureTaps(t)

		trayTapped()

		if got, want := waitForTap(t, opened), "https://unarr.app"; got != want {
			t.Errorf("tap opened %q, want %q", got, want)
		}
	})

	t.Run("honours the UNARR_API_URL override", func(t *testing.T) {
		// A dev tray pointed at a local server must open that server, not the
		// public app — the same override the agent honours.
		t.Setenv("UNARR_API_URL", "http://localhost:3031")
		opened := captureTaps(t)

		trayTapped()

		if got, want := waitForTap(t, opened), "http://localhost:3031"; got != want {
			t.Errorf("tap opened %q, want %q", got, want)
		}
	})
}

func TestTrayTappedDoesNotBlockTheClickPath(t *testing.T) {
	// trayTapped runs inside the platform's click path (the Activate DBus
	// method on Linux, the UI thread on Windows) and browser.OpenURL waits for
	// the process it spawns. If the open ever stops being spawned, that path
	// stays held open for as long as the browser takes to start.
	t.Setenv("UNARR_API_URL", "")

	release := make(chan struct{})
	entered := make(chan struct{})
	prev := tapOpenURL
	tapOpenURL = func(string) {
		close(entered)
		<-release
	}
	t.Cleanup(func() { tapOpenURL = prev })

	returned := make(chan struct{})
	go func() {
		trayTapped()
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("trayTapped blocked on the opener; it must open on its own goroutine")
	}

	// Let the opener finish before the seam is restored, so no goroutine is
	// left holding the swapped var.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the opener never ran")
	}
	close(release)
}
