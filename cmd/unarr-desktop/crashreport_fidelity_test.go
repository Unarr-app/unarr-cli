package main

// Does a crash report actually describe the crash it was sent for?
//
// Two field reports say no. Both arrived from windows/amd64, both were accepted
// by the server, and neither carries the evidence a developer needs:
//
//   - one whose entire log tail is "No logs available. (exit status 0xc0000142)"
//     — the report of a dead agent, whose logs were collected by running a
//     SECOND process that itself failed to start;
//   - one whose header says "Agent: v1.0.4-beta" while its message says
//     "PID 11808, v1.9.0" — one report, two versions for one agent.
//
// The existing handlecrash_test.go cannot catch either: it builds agentStatus
// literals by hand, so readStatus() — the thing that actually populates a real
// crash report — is never on the path under test. These tests go through the
// state file on disk instead.

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// deadAgentState writes the exact on-disk situation that makes readStatus report
// a crash: status "running", a PID that is not alive, and timestamps from after
// the last boot (so StateFromPreviousBoot does not reap it first).
//
// PID 0x7FFFFFFF is above every real pid_max and is never alive on Linux; on
// Windows the same value is not a live process either.
func deadAgentState(t *testing.T, version string) {
	t.Helper()
	writeStateFile(t, agent.DaemonState{
		AgentID:       "agent-under-test",
		Status:        "running",
		Version:       version,
		PID:           0x7FFFFFFF,
		StartedAt:     time.Now().Add(-time.Hour),
		LastHeartbeat: time.Now().Add(-time.Minute),
		LastAlive:     time.Now().Add(-time.Minute),
	})
}

// TestCrashReportVersionIsSelfConsistent: the "Agent:" header and the message
// body describe ONE agent, so they must not disagree.
//
// This is the v1.0.4-beta / v1.9.0 field report. The two numbers are resolved by
// two different functions — handleCrash formats agentStatus.version, sendReport
// sets AgentVersion from resolveAgentVersion() — and nothing pins them together.
func TestCrashReportVersionIsSelfConsistent(t *testing.T) {
	isolatePaths(t)
	srv, reports := crashReportServer(t, http.StatusOK)
	writeAuthConfig(t, srv.URL)
	deadAgentState(t, "1.9.0")
	// The installed binary is NEWER than the daemon that died — the ordinary
	// state after an upgrade the user has not restarted into yet. This is the
	// fallback resolveAgentVersion() reaches for when the state file is gone.
	stubUnarr(t, map[string]reply{
		"version":     {out: "unarr 1.10.1 (windows/amd64)\n"},
		"daemon logs": {out: "2026/08/04 22:45:29 [de608f21] 81%\n"},
	})

	handleCrash(readStatus())

	sent := reports()
	if len(sent) != 1 {
		t.Fatalf("sent %d reports, want exactly 1", len(sent))
	}
	if !strings.Contains(sent[0].Message, sent[0].AgentVersion) {
		t.Errorf("one agent, two versions: header AgentVersion=%q but message says %q.\n"+
			"A developer cannot tell which build actually died.",
			sent[0].AgentVersion, sent[0].Message)
	}
}

// TestCrashReportCarriesEvidenceOrSaysWhyNot: a crash report whose log tail is
// only an error string is not a crash report.
//
// This is the 0xc0000142 field report. The agent died; the tray then exec'd
// `unarr daemon logs` to collect the evidence and THAT process failed to start
// too (STATUS_DLL_INIT_FAILED — the Windows loader kills it before main). The
// report went out anyway, carrying the collector's exit code and nothing about
// the crash.
//
// Sending it is defensible — a failed collection is itself a signal. Sending it
// with no indication that collection FAILED is not: the body reads like a daemon
// that simply had no logs.
func TestCrashReportCarriesEvidenceOrSaysWhyNot(t *testing.T) {
	isolatePaths(t)
	srv, reports := crashReportServer(t, http.StatusOK)
	writeAuthConfig(t, srv.URL)
	deadAgentState(t, "1.10.1")
	// Both collectors fail to launch, exactly as they did in the field.
	stubUnarr(t, map[string]reply{
		"daemon logs":        {err: errors.New("exit status 0xc0000142")},
		"daemon logs --boot": {err: errors.New("exit status 0xc0000142")},
	})

	handleCrash(readStatus())

	sent := reports()
	if len(sent) != 1 {
		t.Fatalf("sent %d reports, want exactly 1", len(sent))
	}
	if strings.Contains(sent[0].Logs, "If the agent runs in the foreground") {
		t.Errorf("log collection FAILED (%q) but the report blames the user's setup:\n%s\n"+
			"A supervised daemon that crashed is not a foreground daemon with no log file.",
			"exit status 0xc0000142", sent[0].Logs)
	}
}

// TestCrashReportLogsAreNotMojibake: the daemon's own log lines reach the report
// as valid UTF-8.
//
// Every field report shows em dashes as "�?\"" — a UTF-8 log written by the
// daemon, read back through the CP1252 console code page. logsources.go already
// keeps the framing it adds pure ASCII for this reason, but the LINES are what a
// developer reads, and they are the ones arriving corrupted.
func TestCrashReportLogsAreNotMojibake(t *testing.T) {
	isolatePaths(t)
	srv, reports := crashReportServer(t, http.StatusOK)
	writeAuthConfig(t, srv.URL)
	deadAgentState(t, "1.10.1")
	// What the daemon actually writes: a real UTF-8 em dash.
	stubUnarr(t, map[string]reply{
		"daemon logs": {out: "2026/08/04 22:45:29 [de608f21] 81% — 3.6 GB/4.4 GB\n"},
	})

	handleCrash(readStatus())

	sent := reports()
	if len(sent) != 1 {
		t.Fatalf("sent %d reports, want exactly 1", len(sent))
	}
	if strings.ContainsRune(sent[0].Logs, '�') {
		t.Errorf("the log tail reached the server with replacement characters:\n%q", sent[0].Logs)
	}
}

// TestCrashReportSurvivesTheStateFileBeingReaped is the field report's actual
// mechanism, and the reason the test above passes while production does not.
//
// handleCrash is handed an agentStatus, formats the message from it — and then
// calls sendReport, which throws that struct away and re-derives the SAME facts
// by reading the state file AGAIN (resolveAgentVersion at support.go:44, and
// agent.ReadState at support.go:48). Three independent reads of one file.
//
// A crashed daemon's state file is deliberately left on disk, but it does not
// stay there: the tray reaps it, and a restart replaces it. When it vanishes
// between the reads, the message keeps the dead agent's version while the header
// falls back to a probe of the INSTALLED binary — which is a different build.
// That is one report describing two agents, and it is what the field saw.
func TestCrashReportSurvivesTheStateFileBeingReaped(t *testing.T) {
	isolatePaths(t)
	srv, reports := crashReportServer(t, http.StatusOK)
	writeAuthConfig(t, srv.URL)
	deadAgentState(t, "1.9.0")

	// Detection happens while the state file is still there.
	s := readStatus()
	if !s.crashed || s.version != "1.9.0" {
		t.Fatalf("setup: readStatus() = %+v, want a crash at 1.9.0", s)
	}

	// The reap the tray itself performs (agentctl.go) — or a restarting daemon —
	// lands before the report is assembled.
	if err := os.Remove(agent.StateFilePath()); err != nil {
		t.Fatalf("reap state file: %v", err)
	}
	stubUnarr(t, map[string]reply{
		"version":     {out: "unarr 1.10.1 (windows/amd64)\n"},
		"daemon logs": {out: "2026/08/04 22:45:29 [de608f21] 81%\n"},
	})

	handleCrash(s)

	sent := reports()
	if len(sent) != 1 {
		t.Fatalf("sent %d reports, want exactly 1", len(sent))
	}
	if sent[0].AgentVersion != "1.9.0" {
		t.Errorf("AgentVersion = %q but the agent that died was 1.9.0 (message: %q).\n"+
			"sendReport re-read the state file instead of using the agentStatus it was given.",
			sent[0].AgentVersion, sent[0].Message)
	}
	if sent[0].AgentID != "agent-under-test" {
		t.Errorf("AgentID = %q, want \"agent-under-test\" — an empty id is what the "+
			"server renders as \"unregistered\"", sent[0].AgentID)
	}
}

// TestCrashReportIsNotSentForACleanStop guards the blast radius of the fixes
// above: a state file whose PID is dead because the daemon recorded a shutdown
// must not mail a crash report. Kept here because those fixes touch exactly the
// readStatus -> handleCrash path this asserts about.
func TestCrashReportIsNotSentForACleanStop(t *testing.T) {
	isolatePaths(t)
	srv, reports := crashReportServer(t, http.StatusOK)
	writeAuthConfig(t, srv.URL)
	writeStateFile(t, agent.DaemonState{
		AgentID:       "agent-under-test",
		Status:        "shutting_down",
		Version:       "1.10.1",
		PID:           0x7FFFFFFF,
		StartedAt:     time.Now().Add(-time.Hour),
		LastHeartbeat: time.Now().Add(-time.Minute),
	})
	stubUnarr(t, map[string]reply{"daemon logs": {out: "x\n"}})

	s := readStatus()
	if s.crashed {
		t.Fatal("a daemon that recorded shutting_down is not a crash")
	}
	if n := len(reports()); n != 0 {
		t.Fatalf("%d report(s) sent for a clean stop", n)
	}
}
