package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// updateTestEnv stubs everything maybeNotifyDesktopUpdate touches: the
// release-version fetch (counting calls), the notifier (recording messages),
// the running version, and an isolated config dir for the throttle state.
// Reuses stubPlayers for the lookPath/statFile/notify seams + config isolation.
type updateTestEnv struct {
	fetches       int
	latest        string
	fetchErr      error
	notifications []string
}

func newUpdateTestEnv(t *testing.T, current, latest string) *updateTestEnv {
	t.Helper()
	stubPlayers(t, runtime.GOOS, nil) // isolates UNARR_CONFIG_DIR + swallows notify
	env := &updateTestEnv{latest: latest}

	prevFetch := fetchLatestRelease
	fetchLatestRelease = func(ctx context.Context) (string, error) {
		env.fetches++
		return env.latest, env.fetchErr
	}
	t.Cleanup(func() { fetchLatestRelease = prevFetch })

	notifySend = func(title, body string) {
		env.notifications = append(env.notifications, title+" | "+body)
	}

	prevVersion := version
	version = current
	t.Cleanup(func() { version = prevVersion })
	return env
}

// backdateCheck rewrites the persisted CheckedAt so the next refresh is
// allowed to hit the (stubbed) network again.
func backdateCheck(t *testing.T) {
	t.Helper()
	st := readUpdateCheckState()
	st.CheckedAt = time.Now().Add(-25 * time.Hour)
	if err := writeUpdateCheckState(st); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshLatestVersionThrottles(t *testing.T) {
	env := newUpdateTestEnv(t, "1.0.0", "1.1.0")

	v, st, dirty := refreshLatestVersion(time.Second)
	if v != "1.1.0" || !dirty {
		t.Fatalf("first refresh = (%q, dirty=%v), want (1.1.0, dirty)", v, dirty)
	}
	// The version fields are the CALLER's to persist now (one combined write).
	persistUpdateCheckState(st, dirty)
	if v, _, dirty := refreshLatestVersion(time.Second); v != "1.1.0" || dirty {
		t.Fatalf("second refresh = (%q, dirty=%v), want cached 1.1.0 and clean", v, dirty)
	}
	if env.fetches != 1 {
		t.Fatalf("fetches = %d, want 1 (24h throttle)", env.fetches)
	}
	backdateCheck(t)
	refreshLatestVersion(time.Second)
	if env.fetches != 2 {
		t.Fatalf("fetches after backdate = %d, want 2", env.fetches)
	}
}

func TestRefreshLatestVersionFetchErrorKeepsThrottle(t *testing.T) {
	env := newUpdateTestEnv(t, "1.0.0", "")
	env.fetchErr = errors.New("rate limited")

	if v, _, dirty := refreshLatestVersion(time.Second); v != "" || dirty {
		t.Fatalf("refresh with fetch error = (%q, dirty=%v), want empty and clean", v, dirty)
	}
	// The failed attempt must still consume the daily slot — no retry storm —
	// even though the caller has nothing to persist (the claim write is
	// internal by design).
	refreshLatestVersion(time.Second)
	if env.fetches != 1 {
		t.Fatalf("fetches = %d, want 1 (failure consumed the slot)", env.fetches)
	}
}

func TestRefreshLatestVersionUnwritableStateSkips(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission-based test needs non-root unix")
	}
	env := newUpdateTestEnv(t, "1.0.0", "1.1.0")
	dir := os.Getenv("UNARR_CONFIG_DIR")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	v, _, _ := refreshLatestVersion(time.Second)
	if v != "" || env.fetches != 0 {
		t.Fatalf("unwritable state: got (%q, %d fetches), want silent skip with 0 fetches", v, env.fetches)
	}
}

func TestMaybeNotifyPersistsCombinedState(t *testing.T) {
	newUpdateTestEnv(t, "1.0.0", "1.1.0")
	maybeNotifyDesktopUpdate()
	// LatestVersion (probe result) and NotifiedVersion (nag marker) must land
	// on disk TOGETHER — the one combined write replacing the old triple.
	st := readUpdateCheckState()
	if st.LatestVersion != "1.1.0" || st.NotifiedVersion != "1.1.0" {
		t.Fatalf("persisted state = %+v, want LatestVersion and NotifiedVersion both 1.1.0", st)
	}
}

func TestMaybeNotifyOncePerVersion(t *testing.T) {
	env := newUpdateTestEnv(t, "1.0.0", "1.1.0")

	maybeNotifyDesktopUpdate()
	if len(env.notifications) != 1 {
		t.Fatalf("notifications = %d, want 1", len(env.notifications))
	}
	// Same play again (throttled AND already-notified) → still one.
	maybeNotifyDesktopUpdate()
	// Force a re-fetch of the SAME version — notified marker must hold.
	backdateCheck(t)
	maybeNotifyDesktopUpdate()
	if len(env.notifications) != 1 {
		t.Fatalf("notifications = %d, want still 1 (once per version)", len(env.notifications))
	}
	// A NEWER version resets the budget.
	env.latest = "1.2.0"
	backdateCheck(t)
	maybeNotifyDesktopUpdate()
	if len(env.notifications) != 2 {
		t.Fatalf("notifications = %d, want 2 after a newer version", len(env.notifications))
	}
	if !strings.Contains(env.notifications[1], "1.2.0") {
		t.Fatalf("second notification %q should mention 1.2.0", env.notifications[1])
	}
}

func TestMaybeNotifySkipsWhenCurrentOrDev(t *testing.T) {
	env := newUpdateTestEnv(t, "1.1.0", "1.1.0")
	maybeNotifyDesktopUpdate()
	if len(env.notifications) != 0 {
		t.Fatalf("up-to-date: notifications = %d, want 0", len(env.notifications))
	}

	env2 := newUpdateTestEnv(t, "dev", "1.1.0")
	maybeNotifyDesktopUpdate()
	if len(env2.notifications) != 0 || env2.fetches != 0 {
		t.Fatalf("dev build: got %d notifications / %d fetches, want 0/0", len(env2.notifications), env2.fetches)
	}
}

func TestMaybeNotifyWordingMatchesInstallShape(t *testing.T) {
	t.Run("player-only suggests --update", func(t *testing.T) {
		env := newUpdateTestEnv(t, "1.0.0", "1.1.0")
		maybeNotifyDesktopUpdate()
		if len(env.notifications) != 1 || !strings.Contains(env.notifications[0], "unarr-desktop --update") {
			t.Fatalf("notifications = %q, want unarr-desktop --update wording", env.notifications)
		}
	})
	t.Run("sibling cli suggests unarr update", func(t *testing.T) {
		env := newUpdateTestEnv(t, "1.0.0", "1.1.0")
		dir := t.TempDir()
		prevSelf := selfExecutablePath
		selfExecutablePath = func() (string, error) { return filepath.Join(dir, "unarr-desktop"), nil }
		t.Cleanup(func() { selfExecutablePath = prevSelf })
		// statFile seam (restored by stubPlayers): only the sibling CLI exists.
		statFile = func(name string) (os.FileInfo, error) {
			if name == filepath.Join(dir, "unarr") {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
		maybeNotifyDesktopUpdate()
		if len(env.notifications) != 1 || !strings.Contains(env.notifications[0], "`unarr update`") {
			t.Fatalf("notifications = %q, want `unarr update` wording", env.notifications)
		}
	})
	t.Run("cli on PATH but not sibling suggests --update", func(t *testing.T) {
		// The regression this criterion exists for: `unarr update` only
		// refreshes a desktop binary in the CLI's OWN dir, so a PATH-only
		// unarr must NOT be recommended — it would "succeed" while leaving
		// this install stale.
		env := newUpdateTestEnv(t, "1.0.0", "1.1.0")
		lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
		maybeNotifyDesktopUpdate()
		if len(env.notifications) != 1 || !strings.Contains(env.notifications[0], "unarr-desktop --update") {
			t.Fatalf("notifications = %q, want unarr-desktop --update wording", env.notifications)
		}
	})
}

func TestUpdateCheckStateRoundTrip(t *testing.T) {
	newUpdateTestEnv(t, "1.0.0", "")
	st := updateCheckState{
		CheckedAt:       time.Now().Truncate(time.Second),
		LatestVersion:   "1.2.3",
		NotifiedVersion: "1.2.3",
	}
	if err := writeUpdateCheckState(st); err != nil {
		t.Fatal(err)
	}
	got := readUpdateCheckState()
	if got.LatestVersion != st.LatestVersion || got.NotifiedVersion != st.NotifiedVersion || !got.CheckedAt.Equal(st.CheckedAt) {
		t.Fatalf("round trip = %+v, want %+v", got, st)
	}
	// Corrupt file → zero state, never a crash.
	if err := os.WriteFile(filepath.Join(os.Getenv("UNARR_CONFIG_DIR"), "desktop-update-check.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readUpdateCheckState(); !got.CheckedAt.IsZero() {
		t.Fatalf("corrupt state = %+v, want zero", got)
	}
}
