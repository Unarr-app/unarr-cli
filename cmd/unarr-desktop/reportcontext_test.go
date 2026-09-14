package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
)

// writeDataLog drops a log file into the sandboxed data dir with a chosen mtime.
func writeDataLog(t *testing.T, name string, mod time.Time) {
	t.Helper()
	path := filepath.Join(config.DataDir(), name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("2026/09/06 00:41:21 last line\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
}

// TestReportContextFlagsADaemonLogOlderThanTheDaemon is the 2026-09-14 field
// report: a daemon alive a minute ago, a daemon log last written eight days
// before, and no log file claimed.
func TestReportContextFlagsADaemonLogOlderThanTheDaemon(t *testing.T) {
	isolatePaths(t)
	now := time.Now()
	writeDataLog(t, fallbackDaemonLogName, now.Add(-8*24*time.Hour))

	got := renderReportContext(reportContext{
		pid:       14112,
		startedAt: now.Add(-2 * time.Hour),
		lastAlive: now.Add(-time.Minute),
	})

	for _, want := range []string{"pid 14112", "none claimed", "STALE"} {
		if !strings.Contains(got, want) {
			t.Errorf("report context does not say %q:\n%s", want, got)
		}
	}
}

// TestReportContextTrustsAFreshDaemonLog: the common case must not cry wolf, and
// an old BOOT log is normal — a healthy run writes nothing there after its banner.
func TestReportContextTrustsAFreshDaemonLog(t *testing.T) {
	isolatePaths(t)
	now := time.Now()
	writeDataLog(t, fallbackDaemonLogName, now.Add(-2*time.Minute))
	writeDataLog(t, fallbackBootLogName, now.Add(-30*24*time.Hour))

	got := renderReportContext(reportContext{
		pid:       9144,
		startedAt: now.Add(-time.Hour),
		lastAlive: now.Add(-time.Minute),
		logFile:   filepath.Join(config.DataDir(), fallbackDaemonLogName),
	})

	if strings.Contains(got, "STALE") {
		t.Errorf("a daemon log written two minutes ago was called stale:\n%s", got)
	}
	if !strings.Contains(got, "claimed by this run") {
		t.Errorf("the claimed log file is not reported:\n%s", got)
	}
}

// TestReportContextTrustsAnIdleDaemonLog: a daemon that has logged nothing for
// hours — idle, with the VPN kill-switch leaving not even DHT bookkeeping to
// write — while LastAlive keeps ticking. Its log was written during this run, so
// it is this run's log, however quiet.
func TestReportContextTrustsAnIdleDaemonLog(t *testing.T) {
	isolatePaths(t)
	now := time.Now()
	writeDataLog(t, fallbackDaemonLogName, now.Add(-5*time.Hour))

	got := renderReportContext(reportContext{
		pid:       7788,
		startedAt: now.Add(-6 * time.Hour),
		lastAlive: now.Add(-10 * time.Second),
		logFile:   filepath.Join(config.DataDir(), fallbackDaemonLogName),
	})

	if strings.Contains(got, "STALE") {
		t.Errorf("an idle daemon's own log was called stale:\n%s", got)
	}
}

// TestReportContextSeesAppendsThroughAnOpenHandle is the live-daemon case: the
// writer keeps unarr.log open, exactly as the daemon does for its whole run.
// NTFS can lag the directory entry's LastWriteTime behind such a writer (seen
// once on the Windows harness, not reproduced), so this only proves something
// when run there — via desktop_test.exe in smoke-resume.ps1, where it passed —
// but it must hold everywhere.
func TestReportContextSeesAppendsThroughAnOpenHandle(t *testing.T) {
	isolatePaths(t)
	path := filepath.Join(config.DataDir(), fallbackDaemonLogName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.WriteString("first line\n"); err != nil {
		t.Fatal(err)
	}
	_, first, ok := statLine(path)
	if !ok {
		t.Fatal("statLine could not read a file that is open for writing")
	}

	time.Sleep(2500 * time.Millisecond)
	if _, err := w.WriteString("second line, same handle\n"); err != nil {
		t.Fatal(err)
	}
	_, second, _ := statLine(path)

	if second.Sub(first) < 2*time.Second {
		t.Errorf("mtime moved %v across a 2.5 s gap with a write in it (first %v, second %v):\n"+
			"a live daemon's log would be reported STALE", second.Sub(first), first, second)
	}
}

// TestReportContextIsASCII: the framing a report adds must survive a CP1252
// viewer, same rule as logsources.go.
func TestReportContextIsASCII(t *testing.T) {
	isolatePaths(t)
	writeDataLog(t, fallbackDaemonLogName, time.Now().Add(-48*time.Hour))
	got := renderReportContext(reportContext{pid: 1, lastAlive: time.Now()})
	for i := 0; i < len(got); i++ {
		if got[i] >= 0x80 {
			t.Fatalf("non-ASCII byte 0x%x at %d in:\n%s", got[i], i, got)
		}
	}
}

// TestCrashReportSaysItsDaemonLogIsStale goes through the real path — state file
// on disk, readStatus, handleCrash, the posted body — so the context is proven
// to carry the run readStatus captured, not a zero one.
func TestCrashReportSaysItsDaemonLogIsStale(t *testing.T) {
	isolatePaths(t)
	srv, reports := crashReportServer(t, http.StatusOK)
	writeAuthConfig(t, srv.URL)
	now := time.Now()
	writeStateFile(t, agent.DaemonState{
		AgentID:       "agent-under-test",
		Status:        "running",
		Version:       "1.11.6",
		PID:           0x7FFFFFFF,
		StartedAt:     now.Add(-time.Hour),
		LastHeartbeat: now.Add(-time.Minute),
		LastAlive:     now.Add(-time.Minute),
	})
	writeDataLog(t, fallbackDaemonLogName, now.Add(-8*24*time.Hour))
	stubUnarr(t, map[string]reply{"daemon logs": {out: "2026/09/06 00:41:21 last line\n"}})

	handleCrash(readStatus())

	sent := reports()
	if len(sent) != 1 {
		t.Fatalf("sent %d reports, want exactly 1", len(sent))
	}
	if !strings.HasPrefix(sent[0].Logs, "===== report context =====") {
		t.Errorf("the report does not open with its context:\n%s", sent[0].Logs)
	}
	if !strings.Contains(sent[0].Logs, "STALE") {
		t.Errorf("an eight-day-old daemon log went out unflagged:\n%s", sent[0].Logs)
	}
}
