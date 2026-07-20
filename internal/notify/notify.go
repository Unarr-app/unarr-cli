// Package notify sends best-effort desktop notifications on the three
// supported platforms. Shared by the engine (download complete/failed) and the
// unarr-desktop tray (agent crash / report status) — one implementation, never
// duplicated per caller.
package notify

import (
	"os"
	"os/exec"
	"runtime"
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
func Send(title, body string) {
	switch runtime.GOOS {
	case "linux":
		spawnAndReap(exec.Command("notify-send", title, body, "--icon=dialog-information", "--app-name=unarr"))
	case "darwin":
		script := `display notification "` + escapeAppleScript(body) + `" with title "` + escapeAppleScript(title) + `"`
		spawnAndReap(exec.Command("osascript", "-e", script))
	case "windows":
		// Use PowerShell toast notification (Windows 10+)
		script := `[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null;` +
			`$xml = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent(1);` +
			`$text = $xml.GetElementsByTagName('text');` +
			`$text[0].AppendChild($xml.CreateTextNode('` + escapePowerShell(title) + `')) > $null;` +
			`$text[1].AppendChild($xml.CreateTextNode('` + escapePowerShell(body) + `')) > $null;` +
			`$toast = [Windows.UI.Notifications.ToastNotification]::new($xml);` +
			`[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('unarr').Show($toast)`
		spawnAndReap(exec.Command("powershell", "-NoProfile", "-Command", script))
	}
}

func escapePowerShell(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\'') // double single-quote to escape
		} else {
			out = append(out, s[i])
		}
	}
	return string(out)
}

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
