package dialog

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

var (
	// ErrCancelled: the user closed the picker without choosing a folder.
	ErrCancelled = errors.New("folder selection cancelled")
	// ErrUnavailable: no picker program could be started (a bare Linux box with
	// neither zenity nor kdialog). The caller must offer another way.
	ErrUnavailable = errors.New("no folder picker available")
)

// PickFolder shows the platform's native folder picker, starting at initial
// when it is an existing directory, and blocks until the user answers. Callers
// run it off any UI path — it waits on a human.
//
// Like Error, it spawns the OS's own dialog program rather than linking a GUI
// toolkit, so the tray stays a small dependency-free binary.
func PickFolder(title, initial string) (string, error) {
	if fi, err := os.Stat(initial); err != nil || !fi.IsDir() {
		initial = ""
	}
	var failed error
	for _, cmd := range folderCandidates(title, initial) {
		winproc.HideWindow(cmd)
		out, err := cmd.Output()
		if err != nil {
			// Exited non-zero quietly = the user cancelled. Non-zero with an error
			// on stderr = the picker broke (a script that did not parse, zenity
			// with no display): try the next one, and report it if none works,
			// rather than looking like a dead menu item. Never started = next.
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				continue
			}
			if msg := pickerFailure(string(ee.Stderr)); msg != "" {
				failed = fmt.Errorf("folder picker failed: %s", msg)
				continue
			}
			return "", ErrCancelled
		}
		// Clean: osascript answers "/Users/x/Media/" — the trailing slash would
		// make an unchanged pick look like a change.
		if dir := strings.TrimSpace(string(out)); dir != "" {
			return filepath.Clean(dir), nil
		}
		return "", ErrCancelled
	}
	if failed != nil {
		return "", failed
	}
	return "", ErrUnavailable
}

// pickerFailure returns the first real error line a picker wrote to stderr, or
// "" when it only said the user cancelled (osascript's -128) or printed toolkit
// warnings (GTK chatter from zenity on an ordinary cancel).
func pickerFailure(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "(-128)") || strings.Contains(strings.ToLower(line), "warning") {
			continue
		}
		return line
	}
	return ""
}

// folderCandidates is a var so tests can stand in for the platform's programs.
var folderCandidates = func(title, initial string) []*exec.Cmd {
	switch runtime.GOOS {
	case "darwin":
		script := `POSIX path of (choose folder with prompt "` + escapeAppleScript(title) + `"`
		if initial != "" {
			script += ` default location POSIX file "` + escapeAppleScript(initial) + `"`
		}
		return []*exec.Cmd{exec.Command("osascript", "-e", script+")")}
	case "windows":
		// UTF-8 stdout: the default OEM code page garbles non-ASCII folder
		// names (a Chinese user's D:\下载) on their way back to Go.
		script := `[Console]::OutputEncoding=[Text.UTF8Encoding]::new($false);` +
			`Add-Type -AssemblyName System.Windows.Forms;` +
			`$d=New-Object System.Windows.Forms.FolderBrowserDialog;` +
			`$d.Description='` + escapePowerShell(title) + `';` +
			`$d.SelectedPath='` + escapePowerShell(initial) + `';` +
			`if($d.ShowDialog() -eq 'OK'){[Console]::Out.Write($d.SelectedPath)}`
		return []*exec.Cmd{exec.Command("powershell", "-NoProfile", "-STA", "-Command", script)}
	default:
		zenity := []string{"--file-selection", "--directory", "--title", title}
		kdialog := []string{"--title", title, "--getexistingdirectory"}
		if initial != "" {
			zenity = append(zenity, "--filename", strings.TrimSuffix(initial, "/")+"/")
			kdialog = append(kdialog, initial)
		}
		return []*exec.Cmd{exec.Command("zenity", zenity...), exec.Command("kdialog", kdialog...)}
	}
}
