//go:build linux

package sysinfo

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPlatformBootTimeParsesProcUptime covers the shapes /proc/uptime can take
// — including the ones a container or a sandbox produces, where "unknown" must
// come back rather than a bogus instant that would make every state file look
// pre-boot.
func TestPlatformBootTimeParsesProcUptime(t *testing.T) {
	cases := []struct {
		name     string
		contents string
		wantOK   bool
		wantAge  time.Duration
	}{
		{"normal", "12345.67 98765.43\n", true, 12345670 * time.Millisecond},
		{"just booted", "0.42 0.30\n", true, 420 * time.Millisecond},
		{"no idle field", "900.00\n", true, 900 * time.Second},
		{"no trailing newline", "60.00 30.00", true, 60 * time.Second},
		{"empty", "", false, 0},
		{"blank", "   \n", false, 0},
		{"not a number", "banana 1.0\n", false, 0},
		{"negative", "-5.0 1.0\n", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "uptime")
			if err := os.WriteFile(path, []byte(tc.contents), 0o600); err != nil {
				t.Fatalf("write fake uptime: %v", err)
			}
			restore := uptimeProcPath
			t.Cleanup(func() { uptimeProcPath = restore })
			uptimeProcPath = path

			before := time.Now()
			got, ok := platformBootTime()
			if ok != tc.wantOK {
				t.Fatalf("platformBootTime() ok = %v, want %v (contents %q)", ok, tc.wantOK, tc.contents)
			}
			if !tc.wantOK {
				return
			}
			// now-uptime, sampled a hair after `before`, so the computed boot
			// sits in [before-age, before-age+slack].
			wantBoot := before.Add(-tc.wantAge)
			if got.Before(wantBoot.Add(-time.Second)) || got.After(wantBoot.Add(5*time.Second)) {
				t.Fatalf("platformBootTime() = %v, want ≈ %v (uptime %v)", got, wantBoot, tc.wantAge)
			}
		})
	}
}

// TestPlatformBootTimeMissingFile: no /proc (a scratch container, a chroot) is
// "unknown", not an error and not a zero instant.
func TestPlatformBootTimeMissingFile(t *testing.T) {
	restore := uptimeProcPath
	t.Cleanup(func() { uptimeProcPath = restore })
	uptimeProcPath = filepath.Join(t.TempDir(), "does-not-exist")

	if got, ok := platformBootTime(); ok {
		t.Fatalf("platformBootTime() = %v, true; want unknown when /proc/uptime is absent", got)
	}
}
