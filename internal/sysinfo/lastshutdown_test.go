package sysinfo

import (
	"testing"
	"time"
)

// TestLastShutdownIsPlausible exercises the REAL platform source. Like
// TestBootTimeIsPlausible it cannot assert a value, so it asserts the
// properties every caller depends on, and emits the reading in a parseable
// shape for the real-Windows harness (test/windows/smoke-boottime.ps1) to diff
// against what Windows itself reports.
//
// Skipping is the normal outcome off Windows: there is no shutdown record to
// read, and callers are built to degrade to the boot-time verdict alone.
func TestLastShutdownIsPlausible(t *testing.T) {
	down, ok := LastShutdown()
	if !ok {
		t.Skip("no shutdown-record source on this platform — callers fall back to BootTime alone")
	}
	now := time.Now()
	if down.After(now) {
		t.Fatalf("LastShutdown() = %v, which is in the future (now %v)", down, now)
	}
	// The FILETIME epoch is 1601. A parse that reads the wrong bytes (or the
	// wrong endianness) lands centuries away, which this catches; a correct one
	// cannot, since Windows did not exist then.
	if down.Year() < 2000 {
		t.Fatalf("LastShutdown() = %v — that is not a shutdown instant, it is a misparsed FILETIME", down)
	}
	t.Logf("lastShutdown=%s", down.UTC().Format(time.RFC3339))

	// The Fast Startup tell, logged rather than asserted: it depends on the
	// host's power settings, and a machine that has never hybrid-shutdown is
	// not a failure. When true, the boot instant carried over a power cycle —
	// which is precisely the case agent.StateFromPreviousBoot cannot see
	// without this source.
	if boot, okBoot := BootTime(); okBoot {
		t.Logf("bootBeforeLastShutdown=%v", boot.Before(down))
	}
}

// TestLastShutdownUsesThePlatformHook guards the seam agent's tests lean on:
// LastShutdown must route through the overridable var, not call the platform
// function directly.
func TestLastShutdownUsesThePlatformHook(t *testing.T) {
	want := time.Date(2026, 8, 26, 0, 2, 20, 0, time.UTC)
	restore := lastShutdownFn
	t.Cleanup(func() { lastShutdownFn = restore })
	lastShutdownFn = func() (time.Time, bool) { return want, true }

	got, ok := LastShutdown()
	if !ok || !got.Equal(want) {
		t.Fatalf("LastShutdown() = %v, %v; want %v, true", got, ok, want)
	}
}
