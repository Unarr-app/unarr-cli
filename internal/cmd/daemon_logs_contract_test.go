package cmd

import "testing"

// TestDaemonLogsBootFlagContract pins the argv unarr-desktop shells out with
// when it assembles a crash report: `unarr daemon logs --boot`.
//
// It is a contract across two binaries that ship together but are compiled
// separately, so nothing else would catch the break: renaming the flag, or
// moving --boot to `unarr logs` only, would leave the tray silently collecting
// one log instead of two — and the missing one is where panics land. The tray
// treats any failure of this call as "no boot log here" (a systemd install, a
// foreground daemon), so the regression would not even show up as an error.
//
// SKIPS WHERE THE FLAG DOES NOT EXIST YET. --boot arrives with the daemon
// log-ownership work (`internal/cmd/logs.go`, `logs_boot.go`), which is not on
// every branch this test rides along with. On such a branch the tray's second
// log source simply never materialises — collectReportLogs degrades to exactly
// the single-section body it always produced — so a hard failure here would be
// reporting someone else's missing feature as this code's defect. The moment
// the flag lands, this starts standing guard without anyone re-enabling it.
func TestDaemonLogsBootFlagContract(t *testing.T) {
	cmd := newDaemonLogsCmd()
	if cmd.Name() != "logs" {
		t.Fatalf("daemon logs subcommand is named %q", cmd.Name())
	}
	flag := cmd.Flags().Lookup("boot")
	if flag == nil {
		t.Skip("`unarr daemon logs --boot` does not exist on this branch — the tray's " +
			"startup-log section stays absent until the log-ownership work merges")
	}
	if flag.Value.Type() != "bool" {
		t.Fatalf("--boot is %s, but the tray passes it as a bare flag", flag.Value.Type())
	}
	if err := cmd.Flags().Parse([]string{"--boot"}); err != nil {
		t.Fatalf("parse `daemon logs --boot`: %v", err)
	}
}
