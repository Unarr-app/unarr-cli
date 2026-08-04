package main

// Self-update for the desktop binary itself, reusing the CLI's
// internal/upgrade machinery (same release resolution, same compiled-in
// ed25519 key, desktop-specific signed manifest). Two entry points:
//
//   - `unarr-desktop --update` (CLI invocation, stdout feedback) — the ONLY
//     update channel for player-only installs that have no `unarr` binary.
//   - The tray's "Update desktop app" menu item (notification feedback).
//
// Either way the running process is NOT replaced in memory: the binary on
// disk changes and the user is told to restart the tray (a --open dispatch
// picks the new binary up automatically on its next invocation, since each
// one is a fresh process).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"fyne.io/systray"

	"github.com/Unarr-app/unarr-cli/internal/upgrade"
)

// selfUpdateTimeout bounds the whole download+verify+swap. Generous — the
// binaries are tens of MB and consumer uplinks vary.
const selfUpdateTimeout = 10 * time.Minute

// appliedVersion is the newest version this PROCESS successfully put on disk
// (tray click or --update). The running tray keeps executing the OLD binary
// after an update, so comparing against the compiled-in `version` is not
// enough: the 6h re-check loop would re-Show() the update item, and a click
// would re-download the same release, forever until a restart. Consulted by
// that loop and by applyDesktopSelfUpdate; written from the click-handler
// goroutine and read from the checker goroutine → atomic.
var appliedVersion atomic.Value // string

func loadAppliedVersion() string {
	v, _ := appliedVersion.Load().(string)
	return v
}

// applyDesktopSelfUpdate is the ONE self-update engine behind both
// `unarr-desktop --update` and the tray's "Update desktop app" item — the two
// used to duplicate this flow and drift (the tray lacked the current==latest
// guard). It resolves the latest release, short-circuits when this build is
// already current or when that release is already on disk (in-memory
// appliedVersion here; UpdateDesktopBinary additionally consults the on-disk
// version marker), and otherwise applies the update. Returns the resolved
// latest version and whether a new binary was actually installed. Progress
// goes through logf — each surface renders it differently (stdout vs
// stderr-prefixed), only the flow is shared.
func applyDesktopSelfUpdate(ctx context.Context, logf func(string)) (latest string, applied bool, err error) {
	logf("Checking latest version...")
	latest, err = upgrade.CheckLatest(ctx)
	if err != nil {
		return "", false, fmt.Errorf("check latest version: %w", err)
	}
	cur := strings.TrimPrefix(version, "v")
	if cur == latest {
		logf(fmt.Sprintf("unarr-desktop v%s is up to date", cur))
		return latest, false, nil
	}
	// Note: a "dev" build is never equal to a release → an explicit update
	// request replaces it with the latest release. That's the point of the
	// flag/menu item; only the passive notifications skip dev builds.
	if loadAppliedVersion() == latest {
		logf(fmt.Sprintf("unarr-desktop v%s is already on disk — restart to load it", latest))
		return latest, false, nil
	}
	exe, err := selfExecutable()
	if err != nil {
		return latest, false, err
	}
	applied, err = upgrade.UpdateDesktopBinary(ctx, exe, latest, logf)
	if err != nil {
		return latest, false, err
	}
	// A marker-skip (applied=false) still means the on-disk binary IS latest —
	// record it either way so the re-check loop stops offering this release.
	appliedVersion.Store(latest)
	return latest, applied, nil
}

// selfExecutable resolves the running binary's real path (symlinks resolved,
// same as the CLI updater) — the file the update swaps.
func selfExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("detect binary: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return exe, nil
}

// runDesktopSelfUpdate implements `unarr-desktop --update`. Exit codes:
// 0 = updated or already current, 1 = failure.
func runDesktopSelfUpdate() int {
	ctx, cancel := context.WithTimeout(context.Background(), selfUpdateTimeout)
	defer cancel()

	cur := strings.TrimPrefix(version, "v")
	latest, applied, err := applyDesktopSelfUpdate(ctx, func(msg string) { fmt.Println(msg) })
	switch {
	case errors.Is(err, upgrade.ErrNoDesktopAssets):
		fmt.Fprintf(os.Stderr, "unarr-desktop: update: %v\n", err)
		fmt.Fprintln(os.Stderr, "This release has no signed desktop assets (yet) - try again in a few minutes, or reinstall via install.sh / install.ps1.")
		return 1
	case err != nil:
		fmt.Fprintln(os.Stderr, "unarr-desktop: update:", err)
		return 1
	case applied:
		fmt.Printf("Updated unarr-desktop v%s → v%s\n", cur, latest)
		fmt.Println("If the tray is currently running it still executes the old version — quit and reopen it.")
	}
	return 0
}

// traySelfUpdate is the "Update desktop app" click handler. Feedback goes
// through desktop notifications (there is no terminal attached to a tray).
// The item is disabled for the duration so a double-click can't run two
// updates over each other; it's hidden for good on success (this tray can't
// get newer than "just updated" without a restart anyway).
func traySelfUpdate(item *systray.MenuItem) {
	item.Disable()
	notifySend("Updating unarr-desktop…", "Downloading and verifying the new version.")

	ctx, cancel := context.WithTimeout(context.Background(), selfUpdateTimeout)
	defer cancel()
	latest, applied, err := applyDesktopSelfUpdate(ctx, func(msg string) {
		fmt.Fprintln(os.Stderr, "unarr-desktop: tray update:", msg)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: tray update:", err)
		notifySend("Update failed", err.Error())
		item.Enable()
		return
	}
	item.Hide()
	if applied {
		notifySend("unarr-desktop updated to v"+latest,
			"Quit and reopen the tray to start using it — the running tray still executes the old version.")
		return
	}
	// Stale menu item clicked after the release was already applied (marker /
	// in-memory short-circuit): nothing downloaded, just close the loop.
	notifySend("unarr-desktop is up to date", "v"+latest+" is already installed.")
}
