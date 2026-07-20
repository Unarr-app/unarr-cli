package dialog

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestErrorCommandsAlwaysOfferSomething(t *testing.T) {
	cmds := errorCommands("title", "body")
	if len(cmds) == 0 {
		t.Fatal("no dialog candidate for this platform")
	}
}

func TestErrorCommandsCarryTheMessage(t *testing.T) {
	// Whatever the platform's dialog program is, the text the user must read
	// has to actually reach it.
	const body = "unarr rejected this agent's key"
	for _, cmd := range errorCommands("unarr agent", body) {
		joined := strings.Join(cmd.Args, " ")
		if !strings.Contains(joined, "unarr rejected this agent") {
			t.Errorf("%s does not carry the message: %v", cmd.Path, cmd.Args)
		}
	}
}

func TestErrorCommandsFallBackOnLinux(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("only Linux has no guaranteed dialog program")
	}
	// A Linux desktop may ship none of these, so more than one is tried before
	// the caller is told to fall back to a notification.
	cmds := errorCommands("t", "b")
	if len(cmds) < 2 {
		t.Fatalf("got %d candidates, want several so a missing zenity is survivable", len(cmds))
	}
	if !strings.Contains(cmds[0].Path, "zenity") && cmds[0].Args[0] != "zenity" {
		t.Errorf("first candidate is %v, want zenity (the most widely installed)", cmds[0].Args)
	}
}

func TestSpawnAndReapReportsAMissingProgram(t *testing.T) {
	// This false is what makes the caller fall back to a notification instead
	// of silently showing nothing.
	if spawnAndReap(exec.Command("unarr-no-such-dialog-program")) {
		t.Error("spawnAndReap reported success for a program that does not exist")
	}
}

func TestSpawnAndReapDoesNotLeaveAZombie(t *testing.T) {
	// The tray is long-lived, so a dismissed dialog that is never waited on
	// stays a zombie for the life of the process.
	reaped := make(chan *os.ProcessState, 1)
	onReap = func(st *os.ProcessState) { reaped <- st }
	defer func() { onReap = nil }()

	if !spawnAndReap(helperCmd()) {
		t.Fatal("the helper process did not start")
	}

	select {
	case st := <-reaped:
		if st == nil {
			t.Fatal("the dialog exited without being waited on: it is a zombie")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the dialog process was never reaped")
	}
}

func TestEscapeAppleScript(t *testing.T) {
	// An unescaped quote would truncate the AppleScript string and the dialog
	// would never appear — the failure would go unreported again.
	tests := []struct{ in, want string }{
		{`say "hi"`, `say \"hi\"`},
		{`back\slash`, `back\\slash`},
		{"plain", "plain"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := escapeAppleScript(tc.in); got != tc.want {
			t.Errorf("escapeAppleScript(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEscapePowerShell(t *testing.T) {
	tests := []struct{ in, want string }{
		{"it's done", "it''s done"},
		{"plain", "plain"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := escapePowerShell(tc.in); got != tc.want {
			t.Errorf("escapePowerShell(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func helperCmd() *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=TestDialogHelperProcess")
	cmd.Env = append(os.Environ(), "GO_DIALOG_HELPER=1")
	return cmd
}

// TestDialogHelperProcess is not a test: it is the child helperCmd spawns.
func TestDialogHelperProcess(t *testing.T) {
	if os.Getenv("GO_DIALOG_HELPER") != "1" {
		t.Skip("helper process; only runs when spawned by helperCmd")
	}
	os.Exit(0)
}
