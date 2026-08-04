package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// crashReportServer stands in for the support endpoint and hands back every
// report it receives. status lets a test make the send fail.
func crashReportServer(t *testing.T, status int) (*httptest.Server, func() []agent.SupportReport) {
	t.Helper()
	var got []agent.SupportReport
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var report agent.SupportReport
		_ = json.NewDecoder(r.Body).Decode(&report)
		got = append(got, report)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []agent.SupportReport { return got }
}

// TestHandleCrashSendsAReportWithTheDeadAgentsIdentity: the report is the whole
// output of this function, and the PID and version in it are how a developer
// ties it to the log tail that comes with it.
func TestHandleCrashSendsAReportWithTheDeadAgentsIdentity(t *testing.T) {
	isolatePaths(t)
	srv, reports := crashReportServer(t, http.StatusOK)
	writeAuthConfig(t, srv.URL)
	stubUnarr(t, map[string]reply{
		"daemon logs":        {out: "2026/08/04 02:09:09 [funnel] retrying\n"},
		"daemon logs --boot": {out: "panic: runtime error: invalid memory address\n"},
	})

	handleCrash(agentStatus{crashed: true, pid: 12272, version: "1.8.1"})

	sent := reports()
	if len(sent) != 1 {
		t.Fatalf("sent %d reports, want exactly 1", len(sent))
	}
	if sent[0].Kind != "crash" {
		t.Errorf("Kind = %q, want crash", sent[0].Kind)
	}
	for _, want := range []string{"12272", "1.8.1", "without a clean shutdown"} {
		if !strings.Contains(sent[0].Message, want) {
			t.Errorf("message %q does not mention %q", sent[0].Message, want)
		}
	}
	if !strings.Contains(sent[0].Logs, "panic: runtime error") {
		t.Error("the crash report went out without the panic, which is the point of sending it")
	}
}

// TestHandleCrashRespectsTelemetryOptOut: UNARR_NO_TELEMETRY=1 means nothing
// leaves the machine. Not "less", not "anonymised" — nothing. A test that only
// checked a flag would not catch a future refactor that notifies AND sends, so
// this asserts against the server's own record.
func TestHandleCrashRespectsTelemetryOptOut(t *testing.T) {
	isolatePaths(t)
	srv, reports := crashReportServer(t, http.StatusOK)
	writeAuthConfig(t, srv.URL)
	stubUnarr(t, map[string]reply{"daemon logs": {out: "secret paths and torrent names\n"}})
	t.Setenv("UNARR_NO_TELEMETRY", "1")

	handleCrash(agentStatus{crashed: true, pid: 4242, version: "1.9.0"})

	if n := len(reports()); n != 0 {
		t.Fatalf("%d report(s) reached the server with telemetry disabled: %+v", n, reports())
	}
}

// TestHandleCrashSurvivesAServerThatRefuses: the tray must stay up when the
// report cannot be delivered. It runs on a goroutine from the render loop, so a
// panic here would take the whole menu down over a failed HTTP call.
func TestHandleCrashSurvivesAServerThatRefuses(t *testing.T) {
	isolatePaths(t)
	srv, reports := crashReportServer(t, http.StatusInternalServerError)
	writeAuthConfig(t, srv.URL)
	stubUnarr(t, map[string]reply{"daemon logs": {out: "x\n"}})

	handleCrash(agentStatus{crashed: true, pid: 1, version: "1.9.0"}) // must not panic

	if n := len(reports()); n != 1 {
		t.Fatalf("the server saw %d attempts, want 1", n)
	}
}

// TestHandleCrashWithoutCredentialsDoesNotPanic: a player-only box has no API
// key, so sendReport fails before any request. handleCrash must degrade to its
// notification rather than taking the tray with it.
func TestHandleCrashWithoutCredentialsDoesNotPanic(t *testing.T) {
	isolatePaths(t)
	stubUnarr(t, map[string]reply{"daemon logs": {out: "x\n"}})

	handleCrash(agentStatus{crashed: true, pid: 1, version: "1.9.0"})
}
