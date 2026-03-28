package engine

import (
	"os/exec"
	"runtime"
)

// desktopNotify sends a best-effort desktop notification.
// Silent failure — never blocks or errors.
func desktopNotify(title, body string) {
	switch runtime.GOOS {
	case "linux":
		exec.Command("notify-send", title, body, "--icon=dialog-information", "--app-name=unarr").Start()
	case "darwin":
		script := `display notification "` + escapeAppleScript(body) + `" with title "` + escapeAppleScript(title) + `"`
		exec.Command("osascript", "-e", script).Start()
	case "windows":
		// Use PowerShell toast notification (Windows 10+)
		script := `[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null;` +
			`$xml = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent(1);` +
			`$text = $xml.GetElementsByTagName('text');` +
			`$text[0].AppendChild($xml.CreateTextNode('` + escapePowerShell(title) + `')) > $null;` +
			`$text[1].AppendChild($xml.CreateTextNode('` + escapePowerShell(body) + `')) > $null;` +
			`$toast = [Windows.UI.Notifications.ToastNotification]::new($xml);` +
			`[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('unarr').Show($toast)`
		exec.Command("powershell", "-NoProfile", "-Command", script).Start()
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
