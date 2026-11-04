// Package dialog shows native error dialogs, for failures the user is standing
// in front of.
//
// It is deliberately separate from internal/notify. A notification is right for
// something that happened on its own (a download finished); a dialog is right
// for something the user just asked for and did not get. A transient toast is
// how "I pressed Resume and nothing happened" gets reported.
//
// Every dialog also offers to send a report, so a user who hits a failure can
// hand the developers the traces instead of the failure dying on their screen.
//
// No GUI toolkit is involved: each platform's own dialog program is spawned,
// the same way notifications are, so the tray stays a small dependency-free
// binary.
package dialog

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// Choice is what the user did with an error dialog.
type Choice int

const (
	// Unavailable: no dialog program could be started. The caller must fall
	// back to a notification — the user has to be told something.
	Unavailable Choice = iota
	// Dismissed: the user closed the dialog.
	Dismissed
	// SendReport: the user asked for the failure to be reported.
	SendReport
)

// sendLabel is the report button's text, and on Linux also the token zenity
// echoes back when it is pressed.
const sendLabel = "Send report"

// Error shows a native error dialog offering to report the failure, and blocks
// until the user answers. Callers run it off any UI path — it waits on a human.
//
// An Unavailable result means no dialog program was available (a bare Linux box
// with neither zenity nor kdialog); the caller is expected to fall back to a
// notification.
func Error(title, body string) Choice {
	return ask(candidates(title, body))
}

// Confirm asks a yes/no question whose safe answer is no, and blocks until the
// user answers. It is true only when the user pressed yesLabel: closing the
// dialog, or having no dialog program at all, means no.
func Confirm(title, body, yesLabel string) bool {
	return ask(confirmCandidates(title, body, yesLabel)) == SendReport
}

// ask runs the first dialog program that starts. pressedSend reports the
// affirmative button, which ask returns as SendReport.
func ask(cs []candidate) Choice {
	for _, c := range cs {
		winproc.HideWindow(c.cmd)
		out, err := c.cmd.Output()
		if err != nil {
			// Only a process that ran and exited non-zero is an answer. Any
			// other error means it never started (not installed), so the next
			// candidate gets a turn — otherwise a missing zenity would look
			// like the user dismissing a dialog they never saw.
			var ee *exec.ExitError
			if !errors.As(err, &ee) || !c.exitMeansAnswer {
				continue
			}
		}
		if c.pressedSend(string(out), err) {
			return SendReport
		}
		return Dismissed
	}
	return Unavailable
}

// candidate is one dialog program and how to read the user's answer out of it.
type candidate struct {
	cmd *exec.Cmd
	// exitMeansAnswer: this program signals the extra button with a non-zero
	// exit, so a non-zero exit is an answer rather than a failure to launch.
	exitMeansAnswer bool
	pressedSend     func(stdout string, err error) bool
}

// candidates is a var so tests can stand in for the platform's dialog programs.
var candidates = func(title, body string) []candidate {
	switch runtime.GOOS {
	case "darwin":
		// AppleScript reports the pressed button on stdout.
		script := `display dialog "` + escapeAppleScript(body) + `" with title "` +
			escapeAppleScript(title) + `" with icon stop buttons {"Close", "` + sendLabel +
			`"} default button "Close"`
		return []candidate{{
			cmd:         exec.Command("osascript", "-e", script),
			pressedSend: func(out string, _ error) bool { return strings.Contains(out, sendLabel) },
		}}
	case "windows":
		// MessageBox has no custom labels, so the question carries the meaning
		// and Yes/No answers it.
		script := `Add-Type -AssemblyName System.Windows.Forms;` +
			`[System.Windows.Forms.MessageBox]::Show('` + escapePowerShell(body) +
			"\n\n" + `Send a diagnostic report to the developers?','` +
			escapePowerShell(title) + `','YesNo','Error')`
		return []candidate{{
			cmd:         exec.Command("powershell", "-NoProfile", "-Command", script),
			pressedSend: func(out string, _ error) bool { return strings.Contains(out, "Yes") },
		}}
	default:
		return []candidate{
			{
				// zenity echoes the extra button's label and exits non-zero.
				cmd: exec.Command("zenity", "--error", "--no-wrap",
					"--title", title, "--text", body, "--extra-button", sendLabel),
				exitMeansAnswer: true,
				pressedSend: func(out string, _ error) bool {
					return strings.TrimSpace(out) == sendLabel
				},
			},
			{
				// kdialog has no extra button; a labelled yes/no asks the same.
				cmd: exec.Command("kdialog", "--title", title,
					"--warningyesno", body+"\n\nSend a report to the developers?",
					"--yes-label", sendLabel, "--no-label", "Close"),
				exitMeansAnswer: true,
				pressedSend:     func(_ string, err error) bool { return err == nil },
			},
			{
				// Last resort: ancient and ugly, but present on X11 boxes with
				// neither of the above. Better than the user seeing nothing.
				cmd: exec.Command("xmessage", "-center",
					"-buttons", "Close:0,"+sendLabel+":2", title+"\n\n"+body),
				exitMeansAnswer: true,
				pressedSend:     func(_ string, err error) bool { return exitCode(err) == 2 },
			},
		}
	}
}

// confirmCandidates is the question form of candidates; every program defaults
// to the negative button, so a stray Enter does not agree to anything.
var confirmCandidates = func(title, body, yesLabel string) []candidate {
	const noLabel = "No"
	switch runtime.GOOS {
	case "darwin":
		script := `display dialog "` + escapeAppleScript(body) + `" with title "` +
			escapeAppleScript(title) + `" buttons {"` + noLabel + `", "` +
			escapeAppleScript(yesLabel) + `"} default button "` + noLabel + `"`
		return []candidate{{
			cmd: exec.Command("osascript", "-e", script),
			pressedSend: func(out string, _ error) bool {
				return strings.Contains(out, "button returned:"+yesLabel)
			},
		}}
	case "windows":
		// MessageBox has no custom labels; yesLabel is dropped and Yes answers.
		script := `Add-Type -AssemblyName System.Windows.Forms;` +
			`[System.Windows.Forms.MessageBox]::Show('` + escapePowerShell(body) + `','` +
			escapePowerShell(title) + `','YesNo','Question','Button2')`
		return []candidate{{
			cmd:         exec.Command("powershell", "-NoProfile", "-Command", script),
			pressedSend: func(out string, _ error) bool { return strings.Contains(out, "Yes") },
		}}
	default:
		pressedOK := func(_ string, err error) bool { return err == nil }
		return []candidate{
			{
				cmd: exec.Command("zenity", "--question", "--no-wrap", "--default-cancel",
					"--title", title, "--text", body,
					"--ok-label", yesLabel, "--cancel-label", noLabel),
				exitMeansAnswer: true,
				pressedSend:     pressedOK,
			},
			{
				// warningyesno is the kdialog form that focuses No.
				cmd: exec.Command("kdialog", "--title", title, "--warningyesno", body,
					"--yes-label", yesLabel, "--no-label", noLabel),
				exitMeansAnswer: true,
				pressedSend:     pressedOK,
			},
			{
				cmd: exec.Command("xmessage", "-center", "-default", noLabel,
					"-buttons", noLabel+":0,"+yesLabel+":2", title+"\n\n"+body),
				exitMeansAnswer: true,
				pressedSend:     func(_ string, err error) bool { return exitCode(err) == 2 },
			},
		}
	}
}

// exitCode is the process's exit status, or -1 when it did not run or was not
// an exit failure at all.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// escapePowerShell escapes s for a single-quoted PowerShell string. PowerShell
// also closes such a string on the typographic quotes ‘ ’ ‚ ‛ (a folder named
// "Bob’s Films" broke the script), so each is doubled like the ASCII one.
func escapePowerShell(s string) string { return psQuoteEscaper.Replace(s) }

var psQuoteEscaper = strings.NewReplacer("'", "''", "‘", "‘‘", "’", "’’", "‚", "‚‚", "‛", "‛‛")

func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}
