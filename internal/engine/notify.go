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
	}
	// Windows: no-op for now
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
