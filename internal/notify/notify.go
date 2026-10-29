// Package notify sends best-effort desktop notifications on the three
// supported platforms. Shared by the engine (download complete/failed) and the
// unarr-desktop tray (agent crash / report status) — one implementation, never
// duplicated per caller.
package notify

import (
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// spawnAndReap starts a notifier and waits for it on its own goroutine.
//
// The wait is what keeps the caller zombie-free: Send is fire-and-forget, but a
// started child that is never waited on stays a zombie until its parent dies —
// and both callers are long-lived (the daemon notifies on every finished
// download, the tray on every failed control). Best-effort throughout: a
// notifier that is missing or fails is not worth reporting.
func spawnAndReap(cmd *exec.Cmd) {
	if err := cmd.Start(); err != nil {
		return // no notifier installed — nothing to report and nothing to reap
	}
	go func() {
		_ = cmd.Wait()
		if onReap != nil {
			onReap(cmd.ProcessState)
		}
	}()
}

// onReap is called with the reaped child's state once it has been waited on.
// Nil in production; a test sets it before spawning so it can verify the reap
// happened without reading cmd.ProcessState while the goroutine writes it.
// The state is nil unless Wait actually ran, which is what the test asserts.
var onReap func(*os.ProcessState)

// Send sends a best-effort desktop notification.
// Silent failure — never blocks or errors.
func Send(title, body string) { send(title, body, false) }

// SendUrgent is Send for things the user must not miss — a failure they have to
// act on. On Linux that means critical urgency, which most desktops keep on
// screen until dismissed instead of fading after a few seconds; a failure that
// fades is how "nothing happened" is reported. The other platforms have no
// equivalent knob, so it degrades to a normal notification.
func SendUrgent(title, body string) { send(title, body, true) }

func send(title, body string, urgent bool) {
	switch runtime.GOOS {
	case "linux":
		args := []string{title, body, "--icon=dialog-information", "--app-name=unarr"}
		if urgent {
			args = append(args, "--urgency=critical")
		}
		cmd := exec.Command("notify-send", args...)
		winproc.HideWindow(cmd)
		spawnAndReap(cmd)
	case "darwin":
		script := `display notification "` + escapeAppleScript(body) + `" with title "` + escapeAppleScript(title) + `"`
		cmd := exec.Command("osascript", "-e", script)
		winproc.HideWindow(cmd)
		spawnAndReap(cmd)
	case "windows":
		cmd := exec.Command("powershell", "-NoProfile", "-Command", windowsToastScript(title, body))
		winproc.HideWindow(cmd)
		spawnAndReap(cmd)
	}
}

// windowsToastScript builds the PowerShell toast (Windows 10+). The text nodes
// are reached with .Item(n), never $text[n]: PowerShell does not index a WinRT
// XmlNodeList, so $text[1] was $null and every toast showed its title with the
// body silently dropped (verified on Win11 in test/windows).
func windowsToastScript(title, body string) string {
	return `[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null;` +
		`$xml = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent(1);` +
		`$text = $xml.GetElementsByTagName('text');` +
		`$text.Item(0).AppendChild($xml.CreateTextNode('` + escapePowerShell(title) + `')) > $null;` +
		`$text.Item(1).AppendChild($xml.CreateTextNode('` + escapePowerShell(body) + `')) > $null;` +
		`$toast = [Windows.UI.Notifications.ToastNotification]::new($xml);` +
		`[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('unarr').Show($toast)`
}

// escapePowerShell escapes s for a single-quoted PowerShell string, doubling
// the typographic quotes ‘ ’ ‚ ‛ too: PowerShell ends the string on them, and a
// path like "Bob’s Films" in a toast body broke the script.
func escapePowerShell(s string) string { return psQuoteEscaper.Replace(s) }

var psQuoteEscaper = strings.NewReplacer("'", "''", "‘", "‘‘", "’", "’’", "‚", "‚‚", "‛", "‛‛")

func escapeAppleScript(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(out)
}
