//go:build windows

// Package winproc suppresses the console window Windows would otherwise
// allocate when a GUI-subsystem process (the tray, built -H=windowsgui) or a
// detached/windowless daemon spawns a console-subsystem child (ffmpeg,
// ffprobe, unrar, 7z, par2, powershell, taskkill, schtasks, cmd, unarr…).
//
// Without CREATE_NO_WINDOW every such exec.Command flashes a black console
// window — dozens of times during a library scan or while scrubbing the
// player. HideWindow covers the transient-window case; CREATE_NO_WINDOW
// guarantees no console is ever created.
package winproc

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

// HideWindow marks cmd so Windows never allocates a console window for it.
// It is a no-op on non-Windows platforms (see winproc_other.go). Call it after
// exec.Command/CommandContext and before Start/Run/Output/CombinedOutput.
//
// CreationFlags is OR-ed, not assigned, so it composes with any detach flags a
// caller sets (e.g. DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP on the daemon).
func HideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
