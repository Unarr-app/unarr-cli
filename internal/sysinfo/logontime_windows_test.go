package sysinfo

import (
	"testing"
	"time"
)

// TestSessionLogonTimeIsThisSession sanity-checks the WTSINFOW binding against
// the other clocks the host keeps: a sign-in happens after the boot and before
// now. A wrong struct offset reads a neighbouring LARGE_INTEGER (or garbage), and
// a local-time/UTC mix-up moves the instant by the timezone offset — on a VM set
// to UTC-7 either lands outside that window. Skips in session 0 (a service or a
// CI runner with no interactive sign-in), where there is no logon to read.
func TestSessionLogonTimeIsThisSession(t *testing.T) {
	logon, ok := SessionLogonTime()
	if !ok {
		t.Skip("no logon time for this session (session 0 / no interactive sign-in)")
	}
	now := time.Now()
	if logon.After(now.Add(time.Minute)) {
		t.Fatalf("logon %v is in the future (now %v): wrong field or a local-time reading", logon, now)
	}
	if boot, bok := BootTime(); bok && logon.Before(boot.Add(-2*time.Minute)) {
		t.Fatalf("logon %v predates boot %v: wrong field or a local-time reading", logon, boot)
	}
	t.Logf("session logon %v (boot %v, age %v)", logon, bootStamp(), now.Sub(logon).Round(time.Second))
}

func bootStamp() string {
	b, ok := BootTime()
	if !ok {
		return "unknown"
	}
	return b.UTC().Format(time.RFC3339)
}
