package sysinfo

import (
	"testing"
	"time"
)

// TestBootTimeIsPlausible is the only test that exercises the REAL platform
// source. It cannot assert a value — the boot instant differs on every box —
// so it asserts the two properties every caller depends on: the boot is in the
// past, and it is not absurdly far in the past. A source that silently returned
// the zero time (the mistake that would make StateFromPreviousBoot judge every
// state file stale) fails the second one.
func TestBootTimeIsPlausible(t *testing.T) {
	boot, ok := BootTime()
	if !ok {
		t.Skip("no boot-time source on this platform/sandbox — callers degrade to their old behaviour")
	}
	now := time.Now()
	if boot.After(now) {
		t.Fatalf("BootTime() = %v, which is in the future (now %v)", boot, now)
	}
	if age := now.Sub(boot); age > 10*365*24*time.Hour {
		t.Fatalf("BootTime() = %v — an uptime of %v means the source is not a boot instant", boot, age)
	}
	// Emitted in a fixed, parseable shape so the real-Windows harness can diff it
	// against what the OS itself reports (Win32_OperatingSystem.LastBootUpTime).
	// Nothing here can check GetTickCount64 against the truth; only that can.
	t.Logf("boot=%s", boot.UTC().Format(time.RFC3339))
}

// TestBootTimeUsesThePlatformHook guards the seam the other packages' tests
// lean on: BootTime must route through the overridable var, not call the
// platform function directly.
func TestBootTimeUsesThePlatformHook(t *testing.T) {
	want := time.Date(2020, 3, 1, 12, 0, 0, 0, time.UTC)
	restore := bootTimeFn
	t.Cleanup(func() { bootTimeFn = restore })
	bootTimeFn = func() (time.Time, bool) { return want, true }

	got, ok := BootTime()
	if !ok || !got.Equal(want) {
		t.Fatalf("BootTime() = %v, %v; want %v, true", got, ok, want)
	}
}
