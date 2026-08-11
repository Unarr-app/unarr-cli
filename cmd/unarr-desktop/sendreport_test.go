package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// TestSendReportBodyCarriesBothLogsBounded is the end of the wire: it asserts
// what the SERVER actually receives.
//
// Every other test here checks a helper. None of them would notice sendReport
// being pointed back at the unbounded collector — a one-word edit that silently
// changes what a developer gets to read in a crash report. This test fails on
// exactly that, because it inspects the posted JSON rather than the helper.
func TestSendReportBodyCarriesBothLogsBounded(t *testing.T) {
	isolatePaths(t)

	var got agent.SupportReport
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/support-report" {
			t.Errorf("posted to %s, want the support-report endpoint", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode report body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	writeAuthConfig(t, srv.URL)

	// A daemon log well over its budget, and a panic in the startup log.
	noisy := strings.Repeat("[funnel] could not start CloudFlare tunnel - retrying in 5m0s\n", 4000)
	stubUnarr(t, map[string]reply{
		"daemon logs":        {out: noisy + "LAST-DAEMON-LINE\n"},
		"daemon logs --boot": {out: "panic: runtime error: invalid memory address\n"},
		"version":            {out: "unarr 1.8.1 (windows/amd64)\n"},
	})

	if err := sendReport("crash", "Agent process (PID 12272, v1.8.1) died",
		agentStatus{crashed: true, pid: 12272, version: "1.8.1", agentID: "test-agent"}); err != nil {
		t.Fatalf("sendReport: %v", err)
	}

	if got.Kind != "crash" {
		t.Errorf("Kind = %q, want crash", got.Kind)
	}
	if !strings.Contains(got.Logs, "panic: runtime error") {
		t.Error("the posted body has no panic in it: the crash report cannot describe the crash")
	}
	if !strings.Contains(got.Logs, "LAST-DAEMON-LINE") {
		t.Error("the posted body lost the tail of the daemon log")
	}
	if len(got.Logs) > maxReportLogBytes {
		t.Errorf("posted %d bytes of logs, over the %d cap", len(got.Logs), maxReportLogBytes)
	}
	// The whole daemon log is ~250 KB; anything near that means the per-section
	// budget was bypassed and only the final blunt tail is doing the work.
	if len(got.Logs) < 1000 {
		t.Errorf("posted only %d bytes of logs — the sections did not make it", len(got.Logs))
	}
}

// TestSendReportTransportsNonASCIIIntact settles where the mojibake in the
// field reports comes from — and it is not this half of the pipe.
//
// A crash report arrived with "windows ?" install cloudflared" where the daemon
// had written an em dash, and the same bytes show up as "ΓÇö" on a Windows
// console (E2 80 94 read as CP437). Both are DECODING faults at the far end, not
// transport ones: the body is JSON over HTTPS, which is UTF-8 by definition, and
// this proves it survives byte for byte — em dash, accents, CJK and all.
//
// So the fix has to be at the source (see internal/logging.TestLogLinesAreASCII),
// because no amount of care in the client or the server stops a CP437 console
// from mis-rendering a UTF-8 file.
// A user's own filenames can still contain anything, hence this guard: whatever
// the daemon does log must reach the developers unchanged.
func TestSendReportTransportsNonASCIIIntact(t *testing.T) {
	isolatePaths(t)

	const tricky = "peliculas/El Niño — Año 2026/Ñandú ⚡ 日本語 «quotes» café"
	var got agent.SupportReport
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
			t.Errorf("Content-Type = %q, want JSON (which is UTF-8 by spec)", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	writeAuthConfig(t, srv.URL)

	stubUnarr(t, map[string]reply{
		"daemon logs": {out: "2026/08/04 01:19:00 [library] scanned " + tricky + "\n"},
		"version":     {out: "unarr 1.9.0 (windows/amd64)\n"},
	})

	if err := sendReport("logs", "user submitted "+tricky, agentStatus{}); err != nil {
		t.Fatalf("sendReport: %v", err)
	}
	if !strings.Contains(got.Logs, tricky) {
		t.Errorf("the log line was altered in transit:\n got %q\nwant it to contain %q", got.Logs, tricky)
	}
	if !strings.Contains(got.Message, tricky) {
		t.Errorf("the message was altered in transit: %q", got.Message)
	}
}

// TestSendReportWithoutCredentialsFails pins the other branch: a never-signed-in
// box must fail loudly enough for handleCrash to fall back to the mail path,
// rather than posting to nowhere and reporting success.
func TestSendReportWithoutCredentialsFails(t *testing.T) {
	isolatePaths(t)
	stubUnarr(t, map[string]reply{"daemon logs": {out: "x\n"}})

	if err := sendReport("crash", "no creds", agentStatus{}); err == nil {
		t.Fatal("sendReport must fail when the agent has no API key")
	}
}

// writeAuthConfig points the tray's client at a test server.
func writeAuthConfig(t *testing.T, apiURL string) {
	t.Helper()
	body := "[auth]\napi_key = \"test-key\"\napi_url = \"" + apiURL + "\"\n\n" +
		"[agent]\nid = \"test-agent\"\n"
	if err := os.WriteFile(sandboxConfigPath(t), []byte(body), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
}
