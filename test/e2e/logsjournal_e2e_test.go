//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// S1-S3 of docs/plans/fase0-manual-checklist.md, as a test instead of a manual
// pass on a systemd box.
//
// What these pin is the ROUTING DECISION, which is the whole substance of those
// three rows: with a unit installed `unarr logs` reads the journal and a stale
// unarr.log must not shadow it (S1, S2); with no unit it falls back to the file
// (S3). Detection is `service.Respawns()` — a plain os.Stat of the unit file
// `unarr daemon install` writes — so a sandboxed HOME flips it truthfully
// without installing anything into the developer's real session.
//
// What they deliberately do NOT do is install a unit, start a daemon, or call
// systemctl. The manual rows say `unarr daemon install` / `unarr stop`, and
// running that here would stop and uninstall the operator's OWN daemon (there
// is usually a live one; that is the point of the tool). Whether journald then
// physically receives the bytes is a systemd guarantee, not ours: the thing
// that has broken before, and the thing this repo can regress, is which SOURCE
// the command chooses.
//
// The residue that stays manual is therefore small and honest: "no double
// logging" (S1) — that a daemon supervised by systemd does not ALSO grow
// unarr.log. That needs a real supervised daemon and is noted as such in the
// checklist.

// ghostLine is the stale-log marker of S2: a line that only exists in a leftover
// unarr.log from an earlier `unarr up`, never in the journal.
const ghostLine = "LINEA-FANTASMA-DE-HACE-SEMANAS"

// seedStaleLog writes the leftover unarr.log of S2. os.WriteFile, not
// appendLines: nothing has created the file yet in these cases.
func seedStaleLog(t *testing.T, s sandbox) {
	t.Helper()
	line := "2026/08/03 10:00:00 [info] " + ghostLine + "\n"
	if err := os.WriteFile(s.logPath(), []byte(line), 0o644); err != nil {
		t.Fatalf("seed stale log: %v", err)
	}
}

// installUnit writes the systemd unit file into the sandbox HOME. Content does
// not matter — detection is by existence, the same artifact installSystemd
// writes and uninstall removes.
func installUnit(t *testing.T, s sandbox) string {
	t.Helper()
	unit := filepath.Join(s.home, ".config", "systemd", "user", "unarr.service")
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatalf("create unit dir: %v", err)
	}
	if err := os.WriteFile(unit, []byte("[Unit]\nDescription=unarr (test fixture)\n"), 0o644); err != nil {
		t.Fatalf("write unit: %v", err)
	}
	return unit
}

// tryCLI is runCLI that tolerates a non-zero exit, returning stdout+stderr and
// the error. `unarr logs` under a unit shells out to journalctl, which may exit
// non-zero — that is a PASS for these cases (it proves journalctl was chosen),
// not a failure.
//
// Note on isolation: journalctl talks to the session bus and does NOT honour the
// sandbox HOME, so on a developer box with a real unarr unit its output is the
// OPERATOR'S OWN journal. Harmless (these cases only read), but it is why no
// assertion here may depend on the journal's CONTENT — only on which source was
// chosen. Asserting on content would pass or fail based on whoever ran it.
func tryCLI(t *testing.T, s sandbox, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"--config", s.cfgPath}, args...)
	cmd := exec.Command(cliBinary(t), full...)
	cmd.Env = s.env()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// S1 + S2 — with a unit installed, `unarr logs` reads the journal, and a stale
// unarr.log sitting right next to it is never read.
func TestLogsPrefersTheJournalOverAStaleFileWhenAUnitIsInstalled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("journald routing is linux-only")
	}
	s := newSandbox(t)
	s.writeConfig(t, "")

	// The stale file of S2: an earlier `unarr up` left it behind, and it is the
	// ONLY place the ghost line exists.
	seedStaleLog(t, s)

	installUnit(t, s)

	out, err := tryCLI(t, s, "logs", "-n", "20")
	if strings.Contains(out, ghostLine) {
		t.Errorf("`unarr logs` served the stale unarr.log while a unit was installed —\n"+
			"the file reader beat the journal and the user would diagnose from a dead log.\noutput:\n%s", out)
	}
	// Not enough on its own: an empty result would also contain no ghost line.
	// Prove journalctl is what ran, so this cannot pass by the command simply
	// producing nothing. `unarr logs --boot` is refused on a systemd box (there
	// is no boot log to read there), and that refusal is only reachable through
	// the journald branch.
	bootOut, bootErr := tryCLI(t, s, "logs", "--boot", "-n", "5")
	if bootErr == nil {
		t.Errorf("`unarr logs --boot` was accepted with a unit installed — the journald "+
			"branch was not taken, so the case above proved nothing.\noutput:\n%s", bootOut)
	}
	t.Logf("routing confirmed: --boot refused (%v); logs err=%v", bootErr, err)
}

// S3 — with no unit on disk, the file reader is used again. The same sandbox
// content that was invisible above must now be served.
func TestLogsFallsBackToTheFileWhenNoUnitIsInstalled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("journald routing is linux-only")
	}
	s := newSandbox(t)
	s.writeConfig(t, "")

	seedStaleLog(t, s)

	out := runCLI(t, s, "logs", "-n", "20")
	if !strings.Contains(out, ghostLine) {
		t.Errorf("`unarr logs` did not read unarr.log with no unit installed —\n"+
			"detection is stuck on something other than the artifact on disk.\noutput:\n%s", out)
	}
}

// S4 — `unarr logs rotate` under systemd is an HONEST no-op: exit 0, and it does
// not manufacture an empty unarr.log just so it has something to rotate.
//
// Exit 0 matters because the user was right to run the command; failing it would
// send them hunting for a problem that does not exist. The side effect matters
// more: a stray empty unarr.log next to a journald install is exactly the stale
// file S2 then has to ignore.
func TestLogsRotateIsAnHonestNoOpUnderSystemd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("journald routing is linux-only")
	}
	s := newSandbox(t)
	s.writeConfig(t, "[daemon]\nlog_max_size_mb = 1\n")
	installUnit(t, s)

	out, err := tryCLI(t, s, "logs", "rotate")
	if err != nil {
		t.Errorf("`unarr logs rotate` exited non-zero with a unit installed: %v\noutput:\n%s", err, out)
	}
	if _, statErr := os.Stat(s.logPath()); statErr == nil {
		t.Errorf("`unarr logs rotate` created %s on a journald box — "+
			"an empty log file conjured up just to have something to rotate is the "+
			"stale file the journal routing then has to ignore", s.logPath())
	}
}

// The pair above only means something if the SAME sandbox flips behaviour when
// the unit appears and disappears. Asserted in one test so a detection that is
// simply always-file or always-journal cannot pass both cases above by accident.
func TestInstallingAndRemovingTheUnitFlipsTheLogSource(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("journald routing is linux-only")
	}
	s := newSandbox(t)
	s.writeConfig(t, "")
	seedStaleLog(t, s)

	// No unit: the file is served.
	if out := runCLI(t, s, "logs", "-n", "20"); !strings.Contains(out, ghostLine) {
		t.Fatalf("baseline: file not served with no unit:\n%s", out)
	}

	// Unit appears: the file stops being served.
	unit := installUnit(t, s)
	if out, _ := tryCLI(t, s, "logs", "-n", "20"); strings.Contains(out, ghostLine) {
		t.Errorf("the file was still served after the unit appeared:\n%s", out)
	}

	// Unit removed (what `unarr daemon uninstall` does): the file is served
	// again. A detection that cached the first answer fails here.
	if err := os.Remove(unit); err != nil {
		t.Fatalf("remove unit: %v", err)
	}
	if out := runCLI(t, s, "logs", "-n", "20"); !strings.Contains(out, ghostLine) {
		t.Errorf("the file was not served again after the unit was removed:\n%s", out)
	}
}
