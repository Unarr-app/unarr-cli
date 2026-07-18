package main

// Update awareness for unarr-desktop.
//
// Player-only installs (no `unarr` CLI) have no daemon heartbeat and no
// updater running for them — the desktop binary lives ~2 seconds per --open
// invocation. So the --open path itself checks for new releases AFTER the
// player has been dispatched (never before: playback latency is sacred), with
// a hard on-disk throttle: at most one network round-trip per 24h and at most
// ONE notification per new version, both recorded in a small state file in
// the config dir. The tray shares the same state/throttle for its
// "Update desktop app" menu item, so the two surfaces never double-check or
// double-nag.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/upgrade"
)

const (
	// updateCheckInterval is the network-check throttle. One probe per day is
	// plenty for a binary whose releases ship every few weeks.
	updateCheckInterval = 24 * time.Hour
	// updateCheckHTTPBudget bounds the --open post-dispatch check: the player
	// is already running, but the ephemeral process should still exit
	// promptly — a slow GitHub API must not hold it for more than ~3s.
	updateCheckHTTPBudget = 3 * time.Second
	// trayUpdateHTTPBudget is the tray's roomier budget — a long-lived
	// process checking in the background has no exit latency to protect.
	trayUpdateHTTPBudget = 15 * time.Second
)

// fetchLatestRelease is a test seam over the CLI updater's version resolution
// (GitHub Releases API with the Hetzner-backed /version fallback).
var fetchLatestRelease = upgrade.CheckLatest

// updateCheckState is the persisted throttle record.
type updateCheckState struct {
	CheckedAt       time.Time `json:"checkedAt"`
	LatestVersion   string    `json:"latestVersion,omitempty"`
	NotifiedVersion string    `json:"notifiedVersion,omitempty"`
}

// updateCheckPath lives in the CONFIG dir (not the data dir the daemon's
// version cache uses) so a player-only install — which may never create a
// data dir — still gets a working throttle, and UNARR_CONFIG_DIR isolates it
// per dev agent, like everything else desktop reads.
func updateCheckPath() string {
	return filepath.Join(config.Dir(), "desktop-update-check.json")
}

func readUpdateCheckState() updateCheckState {
	var st updateCheckState
	data, err := os.ReadFile(updateCheckPath())
	if err != nil {
		return st // first run / unreadable → zero state (stale by definition)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return updateCheckState{} // corrupt → start over
	}
	return st
}

// writeUpdateCheckState persists the throttle record atomically (tmp+rename,
// same pattern as the upgrade package's version cache).
func writeUpdateCheckState(st updateCheckState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	path := updateCheckPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// refreshLatestVersion returns the latest known release version, hitting the
// network at most once per updateCheckInterval. The CheckedAt claim is
// written BEFORE the network call: if the state file is not writable the
// check is skipped entirely (silently) — otherwise every --open invocation
// on a read-only config dir would hammer the releases API with no throttle.
//
// The VERSION fields are only mutated in the returned state, never persisted
// here: the caller folds its own mutations (NotifiedVersion) in and does ONE
// combined write via persistUpdateCheckState — a successful probe used to
// write the file three times (claim, LatestVersion, NotifiedVersion). The
// claim write is the only one that stays separate; it is the anti-hammer
// design, not an accident.
//
// Returns the freshest known version ("" when unknown), the state, and
// whether the state carries unpersisted mutations (dirty).
func refreshLatestVersion(budget time.Duration) (string, updateCheckState, bool) {
	st := readUpdateCheckState()
	if time.Since(st.CheckedAt) < updateCheckInterval {
		return st.LatestVersion, st, false
	}
	st.CheckedAt = time.Now()
	if err := writeUpdateCheckState(st); err != nil {
		// Unwritable state file → no throttle possible → no check. Silent by
		// design: this is a background nicety, not a feature the user asked for.
		return "", st, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	v, err := fetchLatestRelease(ctx)
	if err != nil {
		// Non-fatal (offline, rate-limited...) — the 24h claim above stands,
		// so this failure is not retried until tomorrow. Logged, not silent.
		fmt.Fprintln(os.Stderr, "unarr-desktop: update check:", err)
		return st.LatestVersion, st, false
	}
	st.LatestVersion = v
	return v, st, true
}

// persistUpdateCheckState is the single combined write closing a
// refreshLatestVersion probe (plus whatever fields the caller mutated on
// top). No-op when nothing is dirty — re-writing an unchanged state on every
// cached read would be pointless churn.
func persistUpdateCheckState(st updateCheckState, dirty bool) {
	if !dirty {
		return
	}
	if err := writeUpdateCheckState(st); err != nil {
		// Worst case: the probe (or notification marker) is forgotten and
		// repeated after the next throttle window. Logged, never fatal.
		fmt.Fprintln(os.Stderr, "unarr-desktop: update check: persist state:", err)
	}
}

// maybeNotifyDesktopUpdate runs AFTER a successful player dispatch (the spawn
// already happened; the ≤3s HTTP budget only delays process exit, never
// playback). Notifies at most once per new version, with the right remedy for
// the install shape: CLI next to this binary → `unarr update` (which also
// refreshes this binary); anything else → `unarr-desktop --update`. NEVER
// auto-applies. All state mutations land in ONE combined write at the end.
func maybeNotifyDesktopUpdate() {
	if version == "dev" {
		return // local/dev builds: version compare is meaningless, never nag
	}
	latest, st, dirty := refreshLatestVersion(updateCheckHTTPBudget)
	defer func() { persistUpdateCheckState(st, dirty) }()
	if latest == "" || !upgrade.IsNewer(version, latest) {
		return
	}
	if st.NotifiedVersion == latest {
		return // this version was already announced once — don't nag per play
	}
	body := "Run `unarr-desktop --update` to get v" + latest + "."
	if cliManagesThisInstall() {
		body = "Run `unarr update` to get v" + latest + "."
	}
	notifySend("unarr-desktop update available", body)
	st.NotifiedVersion = latest
	dirty = true
}

// selfExecutablePath is a test seam over selfExecutable — tests fake an
// install dir without relocating the test binary.
var selfExecutablePath = selfExecutable

// cliManagesThisInstall reports whether an `unarr` CLI binary lives in the
// SAME directory as this executable — the exact mirror of the criterion
// upgrade.FindDesktopSibling applies in reverse. Only that CLI's
// `unarr update` refreshes THIS desktop binary (sibling updates are
// same-dir-only, never a PATH search), so an unarr found elsewhere on PATH
// must not be recommended: the user would run it, see a successful CLI
// update, and this install would stay stale — a remedy that doesn't remedy.
// Drives only the notification WORDING; existence is checked, nothing is
// executed.
func cliManagesThisInstall() bool {
	self, err := selfExecutablePath()
	if err != nil {
		return false
	}
	name := "unarr"
	if hostGOOS == "windows" {
		name += ".exe"
	}
	// Existence only (no FileInfo deref): the statFile seam and the real
	// os.Stat both make err==nil mean "there is something at that path", which
	// is all a wording heuristic needs.
	_, err = statFile(filepath.Join(filepath.Dir(self), name))
	return err == nil
}
