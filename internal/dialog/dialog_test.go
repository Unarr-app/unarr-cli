package dialog

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestCandidatesAlwaysOfferSomething(t *testing.T) {
	if len(candidates("title", "body")) == 0 {
		t.Fatal("no dialog candidate for this platform")
	}
}

func TestCandidatesCarryTheMessageAndTheReportOffer(t *testing.T) {
	// Whatever the platform's dialog program is, the text the user must read
	// has to reach it — and so must the offer to report, which is the whole
	// point of showing a dialog instead of a toast.
	const body = "unarr rejected this agent's key"
	for _, c := range candidates("unarr agent", body) {
		joined := strings.Join(c.cmd.Args, " ")
		if !strings.Contains(joined, "unarr rejected this agent") {
			t.Errorf("%s does not carry the message: %v", c.cmd.Path, c.cmd.Args)
		}
		if !strings.Contains(joined, sendLabel) && !strings.Contains(joined, "report") {
			t.Errorf("%s offers no way to send a report: %v", c.cmd.Path, c.cmd.Args)
		}
	}
}

func TestCandidatesFallBackOnLinux(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("only Linux has no guaranteed dialog program")
	}
	// A Linux desktop may ship none of these, so more than one is tried before
	// the caller is told to fall back to a notification.
	cs := candidates("t", "b")
	if len(cs) < 2 {
		t.Fatalf("got %d candidates, want several so a missing zenity is survivable", len(cs))
	}
	if cs[0].cmd.Args[0] != "zenity" {
		t.Errorf("first candidate is %v, want zenity (the most widely installed)", cs[0].cmd.Args)
	}
}

func TestErrorIsUnavailableWhenNothingIsInstalled(t *testing.T) {
	// The bug this guards: a program that is NOT installed must not be read as
	// the user dismissing a dialog they never saw — that would swallow the
	// failure instead of falling back to a notification.
	prev := candidates
	t.Cleanup(func() { candidates = prev })
	candidates = func(_, _ string) []candidate {
		return []candidate{{
			cmd:             exec.Command("unarr-no-such-dialog-program"),
			exitMeansAnswer: true,
			pressedSend:     func(string, error) bool { return true },
		}}
	}

	if got := Error("t", "b"); got != Unavailable {
		t.Errorf("Error() = %v, want Unavailable so the caller notifies instead", got)
	}
}

func TestErrorReadsTheUsersAnswer(t *testing.T) {
	tests := []struct {
		name  string
		press bool
		want  Choice
	}{
		{"user asked to report", true, SendReport},
		{"user closed it", false, Dismissed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prev := candidates
			t.Cleanup(func() { candidates = prev })
			candidates = func(_, _ string) []candidate {
				return []candidate{{
					cmd:             exec.Command("echo", "ignored"),
					exitMeansAnswer: true,
					pressedSend:     func(string, error) bool { return tc.press },
				}}
			}

			if got := Error("t", "b"); got != tc.want {
				t.Errorf("Error() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExitCode(t *testing.T) {
	// xmessage reports the pressed button as an exit status, so reading it
	// wrongly would report the wrong answer.
	if got := exitCode(nil); got != 0 {
		t.Errorf("exitCode(nil) = %d, want 0", got)
	}
	if got := exitCode(errors.New("never ran")); got != -1 {
		t.Errorf("exitCode(non-exit error) = %d, want -1", got)
	}
	err := exec.Command("sh", "-c", "exit 2").Run()
	if got := exitCode(err); got != 2 {
		t.Errorf("exitCode(exit 2) = %d, want 2", got)
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
