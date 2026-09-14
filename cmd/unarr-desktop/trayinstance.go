package main

// One tray per user and config dir.
//
// Nothing stopped a second unarr-desktop from starting — the login autostart
// plus a click on the Start-menu entry was enough — and two trays watch the
// same daemon through the same state file. A field crash report (2026-09-14,
// windows, v1.11.6) arrived twice, 3.7 s apart: same PID, byte-identical logs.
// Each tray deduplicated by PID in its own memory, so neither could know the
// other had already sent it.
//
// The lock is scoped like the daemon's own (config.LockPath): a tray started
// with UNARR_CONFIG_DIR pointing at a dev agent is a different tray and may run
// alongside the production one.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

const trayLockName = "unarr-desktop.lock"

func trayLockPath() string { return filepath.Join(config.Dir(), trayLockName) }

// acquireTrayLock takes the single-instance lock for the life of the process.
// ok is false only when ANOTHER tray holds it, and then this one must exit.
//
// Every other failure — a config dir that cannot be created, a filesystem that
// does not support locks — returns ok with a no-op release. The lock exists to
// stop duplicate reports and icons; a tray that refused to start over a lock it
// could not even create would be a far worse failure than the one it prevents.
func acquireTrayLock() (release func(), ok bool) {
	path := trayLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: single-instance lock:", err)
		return func() {}, true
	}
	lock := flock.New(path)
	locked, err := lock.TryLock()
	if err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: single-instance lock:", err)
		return func() {}, true
	}
	if !locked {
		return nil, false
	}
	return func() { _ = lock.Unlock() }, true
}
