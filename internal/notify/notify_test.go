package notify

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestSpawnAndReapDoesNotLeaveAZombie(t *testing.T) {
	// Both callers are long-lived — the daemon notifies on every finished
	// download, the tray on every failed control — so a notifier that is
	// started and never waited on accumulates zombies for the life of the
	// process. ProcessState is only populated once Wait has reaped the child.
	reaped := make(chan *os.ProcessState, 1)
	onReap = func(st *os.ProcessState) { reaped <- st }
	defer func() { onReap = nil }() // safe: set before the spawn, cleared after it signalled

	spawnAndReap(notifyHelperCmd())

	select {
	case st := <-reaped:
		// A process state only exists once Wait has collected it; without the
		// wait the child would linger as a zombie and this stays nil.
		if st == nil {
			t.Fatal("the notifier exited without being waited on: it is a zombie")
		}
		if !st.Exited() {
			t.Errorf("the notifier was reaped in state %v, want a normal exit", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the notifier process was never reaped")
	}
}

func TestSpawnAndReapSurvivesAMissingNotifier(t *testing.T) {
	// A box with no notifier installed must not take the caller down: Send is
	// best-effort and documented never to fail.
	spawnAndReap(exec.Command("unarr-no-such-notifier-binary"))
}

func TestSendNeverFailsTheCaller(t *testing.T) {
	// Whatever this platform's notifier is (or is not), Send must return
	// quietly — it is called from click handlers and download callbacks.
	Send("unarr test", "body from the unit test")
}

func notifyHelperCmd() *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=TestNotifyHelperProcess")
	cmd.Env = append(os.Environ(), "GO_NOTIFY_HELPER=1")
	return cmd
}

// TestNotifyHelperProcess is not a test: it is the child notifyHelperCmd spawns.
func TestNotifyHelperProcess(t *testing.T) {
	if os.Getenv("GO_NOTIFY_HELPER") != "1" {
		t.Skip("helper process; only runs when spawned by notifyHelperCmd")
	}
	os.Exit(0)
}

func TestEscapePowerShell(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"it's done", "it''s done"},
		{"Tom's 'file'", "Tom''s ''file''"},
		{"no quotes", "no quotes"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := escapePowerShell(tt.input)
			if got != tt.want {
				t.Errorf("escapePowerShell(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestEscapeAppleScript(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{`say "hi"`, `say \"hi\"`},
		{`back\slash`, `back\\slash`},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := escapeAppleScript(tt.input)
			if got != tt.want {
				t.Errorf("escapeAppleScript(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
