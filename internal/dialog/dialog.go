// Package dialog shows native error dialogs, for failures the user is standing
// in front of.
//
// It is deliberately separate from internal/notify. A notification is right for
// something that happened on its own (a download finished); a dialog is right
// for something the user just asked for and did not get. A transient toast is
// how "I pressed Resume and nothing happened" gets reported.
//
// No GUI toolkit is involved: each platform's own dialog binary is spawned, the
// same way notifications are, so the tray stays a small dependency-free binary.
package dialog

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Error shows a native error dialog and reports whether one could be shown.
//
// It returns as soon as the dialog is on screen rather than waiting for it to
// be dismissed — the caller is a supervising goroutine, not something that
// should be pinned to a user's attention span. A false return means no dialog
// program was available (a bare Linux box with neither zenity nor kdialog), and
// the caller is expected to fall back to a notification: the user must always
// end up being told something.
func Error(title, body string) bool {
	for _, candidate := range errorCommands(title, body) {
		if spawnAndReap(candidate) {
			return true
		}
	}
	return false
}

// errorCommands lists the dialog programs to try, best first. Linux is the only
// platform where this can come up empty: macOS always has osascript and Windows
// always has PowerShell, while a Linux desktop may ship neither zenity nor
// kdialog.
func errorCommands(title, body string) []*exec.Cmd {
	switch runtime.GOOS {
	case "darwin":
		script := `display dialog "` + escapeAppleScript(body) + `" with title "` +
			escapeAppleScript(title) + `" with icon stop buttons {"OK"} default button "OK"`
		return []*exec.Cmd{exec.Command("osascript", "-e", script)}
	case "windows":
		script := `Add-Type -AssemblyName System.Windows.Forms;` +
			`[System.Windows.Forms.MessageBox]::Show('` + escapePowerShell(body) + `','` +
			escapePowerShell(title) + `','OK','Error')`
		return []*exec.Cmd{exec.Command("powershell", "-NoProfile", "-Command", script)}
	default:
		return []*exec.Cmd{
			exec.Command("zenity", "--error", "--no-wrap", "--title", title, "--text", body),
			exec.Command("kdialog", "--error", body, "--title", title),
			// Last resort: ancient, ugly, and present on X11 boxes that have
			// neither of the above. Better than the user seeing nothing.
			exec.Command("xmessage", "-center", title+"\n\n"+body),
		}
	}
}

// spawnAndReap starts a dialog and reports whether it started. The wait runs on
// its own goroutine so the dismissed dialog is reaped instead of lingering as a
// zombie for the life of the tray.
func spawnAndReap(cmd *exec.Cmd) bool {
	if err := cmd.Start(); err != nil {
		return false // not installed — try the next candidate
	}
	go func() {
		_ = cmd.Wait()
		if onReap != nil {
			onReap(cmd.ProcessState)
		}
	}()
	return true
}

// onReap is called with the dismissed dialog's state once it has been waited
// on. Nil in production; a test sets it to verify the reap without reading
// cmd.ProcessState while the goroutine writes it.
var onReap func(*os.ProcessState)

func escapePowerShell(s string) string { return strings.ReplaceAll(s, "'", "''") }

func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}
