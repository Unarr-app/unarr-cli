//go:build windows

package sysinfo

import (
	"time"

	"golang.org/x/sys/windows"
)

var (
	kernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procGetTickCount64 = kernel32.NewProc("GetTickCount64")
)

// platformBootTime derives the boot instant from GetTickCount64: milliseconds
// since the system started.
//
// GetTickCount64, NOT QueryUnbiasedInterruptTime — the "unbiased" clocks
// deliberately exclude time spent asleep or hibernating, and this host class is
// a laptop that sleeps every night. An unbiased reading after an eight-hour
// sleep would place the apparent boot eight hours later than the real one, i.e.
// AFTER the state file a genuine crash left behind, and the crash would be
// dismissed as a reboot. Biased (interrupt-time) ticks include the sleep, so
// the instant stays anchored to the real boot. See BootTime.
//
// The tick counter wraps after ~585 million years, so no rollover handling.
//
// 64-BIT TARGETS ONLY. proc.Call returns the result in a uintptr, so the full
// ULONGLONG only fits where uintptr is 64 bits — every arch this project ships
// (.goreleaser.yml: amd64, arm64). On windows/386 the syscall layer would put
// the low half in r1 and the high half in r2, and reading r1 alone would wrap
// every 49.7 days: an uptime that resets under a running daemon, which is the
// exact input that makes StateFromPreviousBoot misjudge. If a 386 target is ever
// added, recombine the halves here — do not let this compile as-is.
func platformBootTime() (time.Time, bool) {
	if err := procGetTickCount64.Find(); err != nil {
		return time.Time{}, false // pre-Vista kernel32, or a stubbed DLL
	}
	ms, _, _ := procGetTickCount64.Call()
	if ms == 0 {
		return time.Time{}, false
	}
	return time.Now().Add(-time.Duration(ms) * time.Millisecond), true
}
