package cmd

import "testing"

// TestIsDaemonEntry pins which commands get SIGPIPE absorbed before the daemon
// runs: only the two that call runDaemon, never `daemon start` (a short-lived
// launcher) or a plain CLI command that should die on a closed pipe.
func TestIsDaemonEntry(t *testing.T) {
	for args, want := range map[string]bool{
		"start": true, "up": true,
		"stop": false, "search": false, "status": false,
	} {
		c, _, err := rootCmd.Find([]string{args})
		if err != nil {
			t.Fatalf("find %q: %v", args, err)
		}
		if got := isDaemonEntry(c); got != want {
			t.Errorf("isDaemonEntry(%q) = %v, want %v", args, got, want)
		}
	}
	c, _, err := rootCmd.Find([]string{"daemon", "start"})
	if err != nil {
		t.Fatal(err)
	}
	if isDaemonEntry(c) {
		t.Error("`daemon start` is a launcher, not the daemon")
	}
}
