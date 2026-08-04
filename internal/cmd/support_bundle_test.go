package cmd

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSupportBundleIsWiredIntoTheSystemGroup: the command only helps if a user
// stuck in a support thread can find it in `unarr --help`.
func TestSupportBundleIsWiredIntoTheSystemGroup(t *testing.T) {
	var found bool
	for _, c := range rootCmd.Commands() {
		if c.Name() != "support-bundle" {
			continue
		}
		found = true
		if c.GroupID != "system" {
			t.Errorf("support-bundle is in group %q, want \"system\"", c.GroupID)
		}
		for _, flag := range []string{"out", "log-lines", "print"} {
			if c.Flags().Lookup(flag) == nil {
				t.Errorf("support-bundle is missing --%s", flag)
			}
		}
	}
	if !found {
		t.Fatal("support-bundle is not registered on the root command")
	}
}

// TestSupportBundleRejectsNegativeLogLines fails before collecting anything —
// a bad flag must not cost the user a doctor run first.
func TestSupportBundleRejectsNegativeLogLines(t *testing.T) {
	err := runSupportBundle(supportBundleOpts{logLines: -1})
	if err == nil || !strings.Contains(err.Error(), "--log-lines") {
		t.Fatalf("want a --log-lines error, got %v", err)
	}
}

// TestBundleLogPathsMatchTheReader pins the bundle to the same files
// `unarr logs` reads. Two readers with two ideas of where the log lives is the
// bug this test exists to prevent.
func TestBundleLogPathsMatchTheReader(t *testing.T) {
	dir := withDataDir(t)

	got := bundleLogPaths(7)
	if got.Daemon != daemonLogPath() {
		t.Errorf("daemon log = %s, want %s (the path `unarr logs` reads)", got.Daemon, daemonLogPath())
	}
	if got.Err != filepath.Join(dir, errLogFileName) {
		t.Errorf("err log = %s, want it under the data dir", got.Err)
	}
	if got.Boot != filepath.Join(dir, bootLogFileName) {
		t.Errorf("boot log = %s, want it under the data dir", got.Boot)
	}
	if got.MaxFiles != 7 {
		t.Errorf("MaxFiles = %d, want the configured log_max_files", got.MaxFiles)
	}
}
