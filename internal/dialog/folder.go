package dialog

import (
	"errors"
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
	for _, cmd := range folderCandidates(title, initial) {
		winproc.HideWindow(cmd)
		out, err := cmd.Output()
		if err != nil {
			// A picker that ran and exited non-zero is a cancel; any other
			// error means it never started, so the next candidate gets a turn.
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return "", ErrCancelled
			}
			continue
		}
		// Clean: osascript answers "/Users/x/Media/" — the trailing slash would
		// make an unchanged pick look like a change.
		if dir := strings.TrimSpace(string(out)); dir != "" {
			return filepath.Clean(dir), nil
		}
		return "", ErrCancelled
	}
	return "", ErrUnavailable
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
