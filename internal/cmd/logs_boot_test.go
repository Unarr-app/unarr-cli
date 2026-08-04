package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/service"
)

// --boot must read the file the SUPERVISOR holds, not the one the daemon owns.
// They are different files by necessity (cmd.exe's FILE_SHARE_READ), so a
// --boot that silently read unarr.log would answer the "it never started"
// question with output from a run that did start.
func TestBuildLogQueryBootReadsTheBootRing(t *testing.T) {
	withDataDir(t)
	withConfig(t, config.Default())

	q, err := buildLogQuery(logsOptions{lines: 10, boot: true})
	if err != nil {
		t.Fatalf("buildLogQuery() = %v", err)
	}
	if q.Path != daemonBootLogPath() {
		t.Errorf("--boot reads %s, want %s", q.Path, daemonBootLogPath())
	}
	// One slot, not [daemon] log_max_files: the boot ring is its own size.
	if q.MaxFiles != bootLogMaxFiles {
		t.Errorf("--boot MaxFiles = %d, want the boot ring's %d", q.MaxFiles, bootLogMaxFiles)
	}

	plain, err := buildLogQuery(logsOptions{lines: 10})
	if err != nil {
		t.Fatalf("buildLogQuery() = %v", err)
	}
	if plain.Path != daemonLogPath() {
		t.Errorf("without --boot the source moved to %s, want %s", plain.Path, daemonLogPath())
	}
}

// The dead end this exists to remove: a daemon that dies before it can open its
// own log leaves a data dir that looks empty, and "no daemon log yet" tells the
// user to do the thing they just did. The boot log holds the actual reason, so
// the error has to point at it — but only when there is something in it, or the
// hint leads nowhere.
func TestMissingDaemonLogErrorPointsAtANonEmptyBootLog(t *testing.T) {
	dir := withDataDir(t)

	silent := missingDaemonLogError(daemonLogPath()).Error()
	if strings.Contains(silent, "--boot") {
		t.Errorf("the error offers --boot with no boot log on disk: %q", silent)
	}

	if err := os.WriteFile(filepath.Join(dir, bootLogFileName), []byte{}, 0o644); err != nil {
		t.Fatalf("seed empty boot log: %v", err)
	}
	if empty := missingDaemonLogError(daemonLogPath()).Error(); strings.Contains(empty, "--boot") {
		t.Errorf("the error offers --boot for a zero-byte boot log: %q", empty)
	}

	if err := os.WriteFile(filepath.Join(dir, bootLogFileName), []byte("panic: boom\n"), 0o644); err != nil {
		t.Fatalf("seed boot log: %v", err)
	}
	got := missingDaemonLogError(daemonLogPath()).Error()
	if !strings.Contains(got, "unarr logs --boot") {
		t.Errorf("the error does not point at the boot log that holds the reason: %q", got)
	}
}

// A rotated slot counts too: the shim renames the boot log aside at its budget,
// so right after a trim the live file can be absent while the evidence is very
// much on disk.
func TestBootLogHasContentSeesTheRotatedSlot(t *testing.T) {
	dir := withDataDir(t)

	if bootLogHasContent() {
		t.Fatal("bootLogHasContent() = true with an empty data dir")
	}
	if err := os.WriteFile(filepath.Join(dir, bootLogFileName+".1"), []byte("panic: boom\n"), 0o644); err != nil {
		t.Fatalf("seed rotated boot log: %v", err)
	}
	if !bootLogHasContent() {
		t.Error("bootLogHasContent() = false with a non-empty rotated slot — the shim's trim would hide the crash")
	}
}

// On a systemd box the unit has no StandardOutput=, so the startup output goes
// to the journal and no boot log is ever written. Falling back to the journal
// would answer a question asked about a file with content from somewhere else;
// an explicit error that names the right command does not.
func TestBootSourceIsRefusedUnderJournald(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("journald detection is linux-only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	// No unit installed: this box writes files, so --boot has a file to read.
	if err := bootSourceUnavailable(); err != nil {
		t.Fatalf("bootSourceUnavailable() = %v with no unit installed, want nil", err)
	}

	// service.Respawns() detects the supervisor purely by this file existing —
	// the same artifact installSystemd writes.
	unit := service.SystemdUnitPathIn(home)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatalf("create unit dir: %v", err)
	}
	if err := os.WriteFile(unit, []byte(systemdTemplate), 0o644); err != nil {
		t.Fatalf("write unit: %v", err)
	}
	if !usesJournald() {
		t.Skip("journald detection did not pick up the unit in this environment")
	}

	err := bootSourceUnavailable()
	if err == nil {
		t.Fatal("--boot was accepted on a systemd box, where no boot log is ever written")
	}
	// The error has to name the command that DOES work, or it is just a refusal.
	if !strings.Contains(err.Error(), "unarr logs") {
		t.Errorf("the refusal does not say where the startup output actually is: %q", err)
	}
}
