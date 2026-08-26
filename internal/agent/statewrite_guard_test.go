package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryStateWriteGoesThroughMutateState is an architectural regression
// guard, in the spirit of winproc.TestEveryExecCommandHidesWindow.
//
// Daemon.State is written from at least five goroutines — the sync loop, the
// funnel supervisor, the VPN reconnect supervisor, the control server and the
// signal handler — and WriteState marshals the WHOLE struct. stateMu is what
// makes that safe, and it only works if every mutation goes through
// mutateState: a single `d.State.X = …; WriteState(&d.State)` added later
// re-opens the race, and the symptom (a heartbeat marshalling "running" back
// over a shutdown mark, so a deliberate stop mails a crash report) is invisible
// in review.
//
// The AST here would be overkill: the pattern is textual and unambiguous.
func TestEveryStateWriteGoesThroughMutateState(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Skip("package sources not available next to the test binary (go test -c run from elsewhere)")
	}
	var checked int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		checked++
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			// The one legitimate site is the call inside mutateState itself,
			// which daemon.go guards with stateMu.
			if strings.Contains(trimmed, "WriteState(&d.State)") && !insideMutateState(string(src), i) {
				t.Errorf("%s:%d writes the daemon state directly: %s\n"+
					"    Use d.mutateState(func(st *DaemonState) { … }) — see the stateMu comment on Daemon.",
					name, i+1, trimmed)
			}
			if strings.Contains(trimmed, "d.State.") && strings.Contains(trimmed, "=") &&
				!strings.Contains(trimmed, "==") && !strings.HasPrefix(trimmed, "//") {
				t.Errorf("%s:%d assigns to d.State outside mutateState: %s\n"+
					"    Mutations must run under stateMu — see the stateMu comment on Daemon.",
					name, i+1, trimmed)
			}
		}
	}
	if checked == 0 {
		t.Skip("no package sources found to check")
	}
}

// insideMutateState reports whether the given line index falls within the body
// of mutateState. Crude on purpose — the function is four lines long and the
// alternative (a full AST walk) buys nothing here.
func insideMutateState(src string, line int) bool {
	lines := strings.Split(src, "\n")
	for i := line; i >= 0 && i > line-8; i-- {
		if strings.HasPrefix(lines[i], "func (d *Daemon) mutateState(") {
			return true
		}
	}
	return false
}
